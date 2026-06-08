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
	Short: "Remove messages from the DB that were deleted in Telegram (reconcile against a chat JSON export)",
	Long: `The Telegram Bot API does not notify the bot when messages are deleted in a
group, so deleted messages linger in the DB and pollute summaries. This command
reconciles the DB against a JSON export of the chat (Telegram Desktop → Export
chat history → JSON format): messages present in the DB but absent from the
export are treated as deleted and removed from the DB.

Dry-run by default (preview only). Add --apply to actually delete; before
deleting, a backup of the removed rows is written to pruned-<group>-<unixtime>.json.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return runPruneDeleted(cmd.Context(), c, pruneFile, pruneGroup, pruneApply)
	},
}

func init() {
	pruneDeletedCmd.Flags().StringVar(&pruneFile, "file", "", "path to the Telegram Desktop JSON export")
	pruneDeletedCmd.Flags().Int64Var(&pruneGroup, "group", 0, "group ID in Telegram format (negative, e.g. -100123456789)")
	pruneDeletedCmd.Flags().BoolVar(&pruneApply, "apply", false, "actually delete (without this flag, preview only)")
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

	// Without --group: list the known groups for the user and exit.
	if groupID == 0 {
		return printKnownGroups(ctx, database)
	}
	if filePath == "" {
		return fmt.Errorf("--file is required (path to the chat JSON export)")
	}

	live, minID, maxID, err := parseExport(filePath)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		return fmt.Errorf("no messages with an id found in export %q", filePath)
	}

	stored, err := database.GetMessagesInTgIDRange(ctx, groupID, minID, maxID)
	if err != nil {
		return fmt.Errorf("reading messages from DB: %w", err)
	}

	toDelete := selectDeletedMessages(live, stored)

	fmt.Printf("Group: %d\n", groupID)
	fmt.Printf("Export: %d messages, message_id range [%d..%d]\n", len(live), minID, maxID)
	fmt.Printf("In DB within this range: %d messages\n", len(stored))
	fmt.Printf("Deleted in Telegram (present in DB, absent from export): %d\n\n", len(toDelete))

	if len(toDelete) == 0 {
		fmt.Println("Nothing to delete.")
		return nil
	}

	fmt.Println("tg_message_id | time (UTC)          | text")
	fmt.Println("--------------+---------------------+------------------------------")
	for _, m := range toDelete {
		fmt.Printf("%13d | %s | %s\n", m.TgMessageID, m.Timestamp.UTC().Format("2006-01-02 15:04:05"), truncate(m.Text, 60))
	}
	fmt.Println()

	if !apply {
		fmt.Println("Dry run. Add --apply to delete.")
		return nil
	}

	backupPath := fmt.Sprintf("pruned-%d-%d.json", groupID, time.Now().Unix())
	if err := writeBackup(backupPath, toDelete); err != nil {
		return fmt.Errorf("writing backup %q: %w", backupPath, err)
	}

	ids := make([]int64, len(toDelete))
	for i, m := range toDelete {
		ids[i] = m.ID
	}
	deleted, err := database.DeleteMessagesByIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("deleting messages: %w", err)
	}

	fmt.Printf("Deleted messages: %d\n", deleted)
	fmt.Printf("Backup: %s\n", backupPath)
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
		return nil, 0, 0, fmt.Errorf("reading export %q: %w", filePath, err)
	}
	var export tgExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, 0, 0, fmt.Errorf("parsing JSON %q: %w", filePath, err)
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
		return fmt.Errorf("reading group list: %w", err)
	}
	if len(groups) == 0 {
		fmt.Println("No known groups.")
		return nil
	}
	fmt.Println("Specify a group with --group. Known groups:")
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
