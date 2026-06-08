package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/metrics"
)

var (
	pruneFile  string
	pruneGroup int64
	pruneApply bool
)

var pruneDeletedCmd = &cobra.Command{
	Use:   "prune-deleted",
	Short: "Удалить из БД сообщения, удалённые в Telegram (сверка с JSON-экспортом чата)",
	Long: `Telegram Bot API не сообщает боту об удалении сообщений в группе, поэтому
удалённые сообщения остаются в БД и попадают в саммаризации. Эта команда сверяет
БД с JSON-экспортом чата (Telegram Desktop → Экспорт истории чата → формат JSON):
сообщения, которых нет в экспорте, считаются удалёнными и убираются из БД.

По умолчанию — сухой прогон (только превью). Для реального удаления добавьте --apply;
перед удалением записывается резервная копия удаляемых строк в pruned-<group>-<unixtime>.json.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return runPruneDeleted(cmd.Context(), c, pruneFile, pruneGroup, pruneApply)
	},
}

func init() {
	pruneDeletedCmd.Flags().StringVar(&pruneFile, "file", "", "путь к JSON-экспорту Telegram Desktop")
	pruneDeletedCmd.Flags().Int64Var(&pruneGroup, "group", 0, "ID группы в формате Telegram (отрицательный, напр. -100123456789)")
	pruneDeletedCmd.Flags().BoolVar(&pruneApply, "apply", false, "реально удалить (без флага — только превью)")
	rootCmd.AddCommand(pruneDeletedCmd)
}

// tgExport is the minimal shape of a Telegram Desktop JSON export we need: the
// per-message native id (and type, used only to skip empty entries defensively).
type tgExport struct {
	Messages []struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"messages"`
}

func runPruneDeleted(ctx context.Context, cfg *config.Config, filePath string, groupID int64, apply bool) error {
	database, err := db.New(cfg.DBPath, metrics.New())
	if err != nil {
		return fmt.Errorf("open database %q: %w", cfg.DBPath, err)
	}
	defer func() { _ = database.Close() }()

	// Без --group: подсказать пользователю известные группы и выйти.
	if groupID == 0 {
		return printKnownGroups(ctx, database)
	}
	if filePath == "" {
		return fmt.Errorf("требуется --file (путь к JSON-экспорту чата)")
	}

	live, minID, maxID, err := parseExport(filePath)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		return fmt.Errorf("в экспорте %q не найдено ни одного сообщения с id", filePath)
	}

	stored, err := database.GetMessagesInTgIDRange(ctx, groupID, minID, maxID)
	if err != nil {
		return fmt.Errorf("чтение сообщений из БД: %w", err)
	}

	toDelete := selectDeletedMessages(live, stored)

	fmt.Printf("Группа: %d\n", groupID)
	fmt.Printf("Экспорт: %d сообщений, диапазон message_id [%d..%d]\n", len(live), minID, maxID)
	fmt.Printf("В БД в этом диапазоне: %d сообщений\n", len(stored))
	fmt.Printf("Удалено в Telegram (есть в БД, нет в экспорте): %d\n\n", len(toDelete))

	if len(toDelete) == 0 {
		fmt.Println("Нечего удалять.")
		return nil
	}

	fmt.Println("tg_message_id | время (UTC)         | текст")
	fmt.Println("--------------+---------------------+------------------------------")
	for _, m := range toDelete {
		fmt.Printf("%13d | %s | %s\n", m.TgMessageID, m.Timestamp.UTC().Format("2006-01-02 15:04:05"), truncate(m.Text, 60))
	}
	fmt.Println()

	if !apply {
		fmt.Println("Сухой прогон. Для удаления добавьте флаг --apply.")
		return nil
	}

	backupPath := fmt.Sprintf("pruned-%d-%d.json", groupID, time.Now().Unix())
	if err := writeBackup(backupPath, toDelete); err != nil {
		return fmt.Errorf("запись резервной копии %q: %w", backupPath, err)
	}

	ids := make([]int64, len(toDelete))
	for i, m := range toDelete {
		ids[i] = m.ID
	}
	deleted, err := database.DeleteMessagesByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("удаление сообщений: %w", err)
	}

	fmt.Printf("Удалено сообщений: %d\n", deleted)
	fmt.Printf("Резервная копия: %s\n", backupPath)
	return nil
}

// selectDeletedMessages returns the stored messages whose Telegram message_id is
// absent from the live set (i.e. deleted in Telegram). Messages without a
// tg_message_id (== 0) are never selected — they cannot be reconciled.
func selectDeletedMessages(live map[int64]bool, stored []db.Message) []db.Message {
	var out []db.Message
	for _, m := range stored {
		if m.TgMessageID == 0 {
			continue
		}
		if !live[m.TgMessageID] {
			out = append(out, m)
		}
	}
	return out
}

// parseExport reads a Telegram Desktop JSON export and returns the set of live
// message ids together with their min/max (the id range the export covers).
func parseExport(filePath string) (live map[int64]bool, minID, maxID int64, err error) {
	data, err := os.ReadFile(filePath) // #nosec G304 -- operator-supplied path for a one-off maintenance command
	if err != nil {
		return nil, 0, 0, fmt.Errorf("чтение экспорта %q: %w", filePath, err)
	}
	var export tgExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, 0, 0, fmt.Errorf("разбор JSON %q: %w", filePath, err)
	}
	live = make(map[int64]bool, len(export.Messages))
	for _, m := range export.Messages {
		if m.ID == 0 {
			continue
		}
		if len(live) == 0 || m.ID < minID {
			minID = m.ID
		}
		if m.ID > maxID {
			maxID = m.ID
		}
		live[m.ID] = true
	}
	return live, minID, maxID, nil
}

func writeBackup(path string, msgs []db.Message) error {
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func printKnownGroups(ctx context.Context, database *db.DB) error {
	groups, err := database.GetKnownGroups(ctx)
	if err != nil {
		return fmt.Errorf("чтение списка групп: %w", err)
	}
	if len(groups) == 0 {
		fmt.Println("Известных групп нет.")
		return nil
	}
	fmt.Println("Укажите группу через --group. Известные группы:")
	for _, g := range groups {
		fmt.Printf("  %d  %s\n", g.GroupID, g.Title)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
