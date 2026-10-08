package provider

import (
	"bufio"
	"bytes"
	"io"

	"github.com/openai/openai-go/packages/ssestream"
	"telegram_summarize_bot/logger"
)

// The ChatGPT Codex backend interleaves keepalive blocks (": keepalive\n\n",
// "event: ping\n\n") into its Responses SSE stream. The decoder bundled with
// openai-go v1 dispatches such blocks as events with empty data, and the SDK
// then fails the whole stream with "unexpected end of JSON input". Upstream
// fixed this in v3.42 (openai/openai-go#621); until the SDK is upgraded, the
// tolerant decoder below replaces the built-in one for event-stream bodies.
func init() {
	for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8"} {
		ssestream.RegisterDecoder(ct, newTolerantSSEDecoder)
	}
}

// tolerantSSEDecoder parses text/event-stream, skipping blocks without data.
type tolerantSSEDecoder struct {
	evt ssestream.Event
	rc  io.ReadCloser
	scn *bufio.Scanner
	err error
}

func newTolerantSSEDecoder(rc io.ReadCloser) ssestream.Decoder {
	scn := bufio.NewScanner(rc)
	scn.Buffer(nil, bufio.MaxScanTokenSize<<9)
	return &tolerantSSEDecoder{rc: rc, scn: scn}
}

func (d *tolerantSSEDecoder) Next() bool {
	if d.err != nil {
		return false
	}

	event := ""
	var data []byte

	for d.scn.Scan() {
		line := d.scn.Bytes()

		// A blank line terminates the block. Blocks that carried no data
		// (comments, "event: ping", stray blank lines) are keepalives: drop
		// them instead of handing empty JSON to the SDK.
		if len(line) == 0 {
			if len(data) == 0 {
				if event != "" {
					logger.Debug().Str("event_type", event).Msg("skipping SSE block without data")
				}
				event = ""
				continue
			}
			d.evt = ssestream.Event{Type: event, Data: data}
			return true
		}

		name, value, _ := bytes.Cut(line, []byte(":"))
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(name) {
		case "": // ": comment"
			continue
		case "event":
			event = string(value)
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
		}
	}

	if err := d.scn.Err(); err != nil {
		d.err = err
	}
	return false
}

func (d *tolerantSSEDecoder) Event() ssestream.Event { return d.evt }

func (d *tolerantSSEDecoder) Close() error { return d.rc.Close() }

func (d *tolerantSSEDecoder) Err() error { return d.err }
