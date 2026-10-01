package forward

import (
	"context"
	"testing"
	"time"

	"github.com/Tuskira/claude-desktop-utility/internal/capture"
	"github.com/Tuskira/claude-desktop-utility/internal/claudedesktop"
)

// Regression for the live forwarding path (Sep 30, 2026). Records flow
// exactly as in `interceptor run --gateway-url ...`: capture.Logger.Log ->
// Logger.Consumer (claudedesktop.Converter.Handle) -> Forwarder -> gateway.
// The traffic shape mirrors the live capture: the stream open when the
// message is sent replays conversation history and then closes, the reply
// arrives on a NEW stream on the same connection, and that stream stays
// open (no trailer). Exactly one record must reach the gateway: the prompt
// plus its reply, with no history text.
func TestLiveConsumerPath_ChatReplyPairedByMessageID(t *testing.T) {
	const (
		stURL    = "https://claude.ai/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/StreamTimeline"
		paURL    = "https://claude.ai/claudeai-rpc/anthropic.bard.api.v1alpha.ConversationService/PerformAction"
		client   = "127.0.0.1:56704"
		convID   = "11111111-2222-3333-4444-555555555555"
		oldHuman = "aaaaaaaa-0000-4000-8000-000000000001"
		oldAsst  = "aaaaaaaa-0000-4000-8000-000000000002"
		human    = "bbbbbbbb-0000-4000-8000-000000000003"
		asst     = "bbbbbbbb-0000-4000-8000-000000000004"
		keepAlv  = `{"1":{"6":""}}`
	)
	fg := newFakeGateway(t)
	var conv *claudedesktop.Converter
	f := newTestForwarder(t, fg.srv.URL, func(c *Config) {
		c.UserLabel = func() string { return conv.UserLabel() } // as main.go wires it
	})
	conv = claudedesktop.New(claudedesktop.Config{Sink: f})
	logger := &capture.Logger{Consumer: conv.Handle} // Out/Stdout nil: consumer path only

	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	defer func() {
		cancel()
		f.Close(2 * time.Second)
	}()

	t0 := time.Date(2026, 9, 30, 15, 22, 13, 0, time.Local)
	seq := 0
	unit := func(at time.Duration, decoded string) {
		seq++
		logger.Log(&capture.Record{
			TS: t0.Add(at), Mode: capture.ModeStream, Client: client, Method: "RPC<",
			URL: stURL, Status: 200, Seq: seq, RespBodyDecoded: []byte(decoded),
		})
	}
	performAction := func(at time.Duration, decoded string) {
		logger.Log(&capture.Record{
			TS: t0.Add(at), Mode: capture.ModeProxy, Client: client, Method: "POST",
			URL: paURL, Status: 200, ReqBodyDecoded: []byte(decoded),
		})
	}
	history := `{"1":{"1":{"2":{"1":"` + convID + `","6":{"2":"claude-opus-5-5"}},` +
		`"3":[{"1":"` + oldHuman + `","3":1,"6":1},{"1":"` + oldAsst + `","3":2,"6":1,"11":"` + oldHuman + `"}],` +
		`"4":[{"1":"dgrp_h","2":"` + oldHuman + `"},{"1":"dgrp_a","2":"` + oldAsst + `"}],` +
		`"5":[{"1":"cblk_h","2":"dgrp_h","9":"old question"},{"1":"cblk_a","2":"dgrp_a","9":"history text"}]}}}`

	// Conversation-open PerformAction (no field 2): not a send.
	performAction(0, `{"1":{"2":"`+convID+`","3":3},"15":{"1":{"12":"x"}}}`)
	unit(400*time.Millisecond, history)
	unit(500*time.Millisecond, keepAlv)
	unit(5*time.Second, `{"trailer":{"metadata":{"Stream-Close-Reason":["closed"]}}}`)
	logger.Log(&capture.Record{TS: t0, Mode: capture.ModeProxy, Client: client, Method: "POST", URL: stURL, Status: 200, StreamUnits: seq})
	seq = 0

	// The send: 2.1 human message, 2.2 reply message, 2.3 prompt.
	performAction(5100*time.Millisecond, `{"1":{"2":"`+convID+`","3":3},"2":{"1":"`+human+`","2":"`+asst+`","3":"new prompt"}}`)
	unit(5800*time.Millisecond, history)
	unit(6*time.Second, `{"1":{"1":{"3":{"1":"`+asst+`","3":2,"11":"`+human+`"}}}}`)
	unit(6100*time.Millisecond, `{"1":{"1":{"4":{"1":"dgrp_r","2":"`+asst+`"},"5":{"1":"cblk_r","2":"dgrp_r","9":"Hel"}}}}`)
	unit(6200*time.Millisecond, `{"1":{"2":{"1":"cblk_r","2":"lo"}}}`)
	unit(6300*time.Millisecond, `{"1":{"1":{"3":{"1":"`+asst+`","3":2,"6":1,"11":"`+human+`"}}}}`)
	unit(20*time.Second, keepAlv) // the stream stays open

	deadline := time.Now().Add(5 * time.Second)
	for fg.Count() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	var calls []strictLLMCall
	for _, b := range fg.Received() {
		calls = append(calls, b.LLMCalls...)
	}
	if len(calls) != 1 {
		t.Fatalf("gateway received %d llm_calls, want exactly 1 (the reply; history must not be sent)", len(calls))
	}
	c := calls[0]
	if c.RequestID != "icp_chat_"+asst {
		t.Errorf("request_id = %q, want icp_chat_%s", c.RequestID, asst)
	}
	if c.SessionID != convID {
		t.Errorf("session_id = %q, want the send's conversation id", c.SessionID)
	}
	if want := `{"content":[{"text":"Hello","type":"text"}],"id":"` + asst + `","model":"claude-opus-5-5","role":"assistant","type":"message"}`; c.ResponseBody != want {
		t.Errorf("response_body = %q, want %q", c.ResponseBody, want)
	}
	if want := `{"messages":[{"content":"new prompt","role":"user"}],"model":"claude-opus-5-5"}`; c.RequestBody != want {
		t.Errorf("request_body = %q, want %q", c.RequestBody, want)
	}
	if want := t0.Add(5100 * time.Millisecond).UTC().Format(time.RFC3339Nano); c.Timestamp != want {
		t.Errorf("timestamp = %q, want the send time %q", c.Timestamp, want)
	}
	if c.DurationMS == nil || *c.DurationMS != 1100 {
		t.Errorf("duration_ms = %v, want 1100 (send to last reply text)", c.DurationMS)
	}
}
