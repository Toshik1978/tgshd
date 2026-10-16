package notify

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// chanT0 is the reference time of these tests, 16 Oct 2026 10:00 UTC.
func chanT0() time.Time { return time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC) }

func TestChannelHealthTransitions(t *testing.T) {
	const after = 15 * time.Minute

	var h ChannelHealth
	if h.Failing() {
		t.Fatal("zero ChannelHealth must be OK")
	}

	// OK ⇒ Failing(since) on a failed send or a message queued while offline.
	h.Fail(chanT0())
	if !h.Failing() || !h.FailingSince.Equal(chanT0()) {
		t.Fatalf("after Fail: %+v, want failing since %v", h, chanT0())
	}

	// A later failure keeps the first since.
	h.Fail(chanT0().Add(5 * time.Minute))
	if !h.FailingSince.Equal(chanT0()) {
		t.Fatalf("second Fail moved since to %v", h.FailingSince)
	}

	if h.NoticeDue(chanT0().Add(after-time.Second), after) {
		t.Fatal("notice due before telegram_failing_after")
	}
	if !h.NoticeDue(chanT0().Add(after), after) {
		t.Fatal("notice not due at telegram_failing_after")
	}

	// Failing ⇒ Reported after the notice: no second notice.
	h.MarkReported()
	if !h.Reported || h.NoticeDue(chanT0().Add(time.Hour), after) {
		t.Fatalf("after MarkReported: %+v, notice still due", h)
	}

	// Any successful send ⇒ OK, Reported cleared.
	h.Succeed()
	if h.Failing() || h.Reported || !h.FailingSince.IsZero() {
		t.Fatalf("after Succeed: %+v, want OK", h)
	}

	// MarkReported on an OK channel does nothing; an OK channel is never due.
	h.MarkReported()
	if h.Reported || h.NoticeDue(chanT0().Add(time.Hour), after) {
		t.Fatalf("OK channel: %+v", h)
	}

	// A new failure after recovery starts a new episode, notice due again.
	h.Fail(chanT0().Add(2 * time.Hour))
	if !h.NoticeDue(chanT0().Add(2*time.Hour+after), after) {
		t.Fatal("new failure episode: notice not due")
	}
}

func TestChannelsCodec(t *testing.T) {
	empty, err := DecodeChannels("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("DecodeChannels(\"\") = %v, %v", empty, err)
	}

	in := Channels{
		ChannelTelegram: {FailingSince: chanT0(), Reported: true},
		ChannelEmail:    {},
	}

	s, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	out, err := DecodeChannels(s)
	if err != nil {
		t.Fatalf("DecodeChannels: %v", err)
	}

	tg := out[ChannelTelegram]
	if !tg.FailingSince.Equal(chanT0()) || !tg.Reported {
		t.Fatalf("telegram round trip = %+v", tg)
	}
	if em, ok := out[ChannelEmail]; !ok || em.Failing() || em.Reported {
		t.Fatalf("email round trip = %+v, %v", em, ok)
	}

	if _, err := DecodeChannels("{"); err == nil {
		t.Fatal("malformed channels decoded without error")
	}
}

func TestSMSProblemsSet(t *testing.T) {
	var s SMSProblems
	if s.Has("ups/battery") {
		t.Fatal("empty set has a key")
	}

	s.Add("ups/battery")
	s.Add("disks/TEST0000000001/health")
	s.Add("ups/battery")

	if !s.Has("ups/battery") || s.Len() != 2 {
		t.Fatalf("after Add: len %d", s.Len())
	}

	enc, err := s.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if want := `["disks/TEST0000000001/health","ups/battery"]`; enc != want {
		t.Fatalf("Encode = %s, want %s", enc, want)
	}

	if !s.Remove("ups/battery") || s.Remove("ups/battery") || s.Has("ups/battery") {
		t.Fatal("Remove must report membership once")
	}

	back, err := DecodeSMSProblems(enc)
	if err != nil {
		t.Fatalf("DecodeSMSProblems: %v", err)
	}
	if !back.Has("ups/battery") || !back.Has("disks/TEST0000000001/health") || back.Len() != 2 {
		t.Fatalf("round trip lost keys: len %d", back.Len())
	}

	empty, err := DecodeSMSProblems("")
	if err != nil || empty.Len() != 0 {
		t.Fatalf("DecodeSMSProblems(\"\") = %d, %v", empty.Len(), err)
	}
	if _, err := DecodeSMSProblems("nope"); err == nil {
		t.Fatal("malformed set decoded without error")
	}
}

func item(i int) UndeliveredProblem {
	return UndeliveredProblem{
		Key:       fmt.Sprintf("command/job%02d", i),
		Text:      fmt.Sprintf("job%02d failed", i),
		Since:     chanT0().Add(time.Duration(i) * time.Minute),
		DroppedAt: chanT0().Add(24 * time.Hour),
	}
}

func TestUndeliveredKeptUntilShown(t *testing.T) {
	var u Undelivered
	if text, shown := u.Prefix(MaxMessageUnits, chanT0()); text != "" || shown != (Shown{}) {
		t.Fatalf("empty list Prefix = %q, %+v", text, shown)
	}

	u.Add(item(1))
	u.Add(item(2))

	// A prefix that was built but whose delivery failed is not acknowledged:
	// the items stay.
	text, shown := u.Prefix(MaxMessageUnits, chanT0().Add(time.Hour))
	want := "Undelivered earlier:\n" +
		"PROBLEM: job01 failed (since 10:01), no longer open\n" +
		"PROBLEM: job02 failed (since 10:02), no longer open\n\n"
	if text != want {
		t.Fatalf("Prefix =\n%q\nwant\n%q", text, want)
	}
	if shown != (Shown{Items: 2}) {
		t.Fatalf("shown = %+v", shown)
	}
	if len(u.Items) != 2 {
		t.Fatalf("Prefix changed the list: %d items", len(u.Items))
	}

	// Added while the delivery was in flight: kept after the Ack.
	u.Add(item(3))
	u.Ack(shown)

	if len(u.Items) != 1 || u.Items[0].Key != item(3).Key {
		t.Fatalf("after Ack: %+v", u.Items)
	}

	// An earlier day shows the date; text is HTML-escaped; zero since omits it.
	u = Undelivered{}
	u.Add(UndeliveredProblem{Key: "a", Text: "x < y & z", Since: chanT0()})
	u.Add(UndeliveredProblem{Key: "b", Text: "no since"})
	text, _ = u.Prefix(MaxMessageUnits, chanT0().Add(48*time.Hour))
	want = "Undelivered earlier:\n" +
		"PROBLEM: x &lt; y &amp; z (since 16 Oct 10:00), no longer open\n" +
		"PROBLEM: no since, no longer open\n\n"
	if text != want {
		t.Fatalf("Prefix =\n%q\nwant\n%q", text, want)
	}
}

func TestUndeliveredLinesCarryOver(t *testing.T) {
	var u Undelivered
	for i := 1; i <= 3; i++ {
		u.Add(item(i))
	}

	header := "Undelivered earlier:\n"
	line := func(i int) string {
		return fmt.Sprintf("PROBLEM: job%02d failed (since 10:%02d), no longer open\n", i, i)
	}

	// Room for the header, two lines and the separator only.
	limit := utf16Len(header + line(1) + line(2) + "\n")

	text, shown := u.Prefix(limit, chanT0())
	if want := header + line(1) + line(2) + "\n"; text != want {
		t.Fatalf("Prefix =\n%q\nwant\n%q", text, want)
	}
	if utf16Len(text) > limit {
		t.Fatalf("prefix %d units over limit %d", utf16Len(text), limit)
	}

	u.Ack(shown)

	if len(u.Items) != 1 || u.Items[0].Key != item(3).Key {
		t.Fatalf("the line that did not fit must stay: %+v", u.Items)
	}

	text, shown = u.Prefix(MaxMessageUnits, chanT0())
	if want := header + line(3) + "\n"; text != want {
		t.Fatalf("next Prefix = %q, want %q", text, want)
	}
	u.Ack(shown)
	if len(u.Items) != 0 {
		t.Fatalf("after second Ack: %+v", u.Items)
	}

	// Too small for even one line: nothing shown, nothing removed.
	u.Add(item(4))
	text, shown = u.Prefix(utf16Len(header), chanT0())
	if text != "" || shown != (Shown{}) {
		t.Fatalf("tiny limit Prefix = %q, %+v", text, shown)
	}
	u.Ack(shown)
	if len(u.Items) != 1 {
		t.Fatalf("tiny limit Ack removed items: %+v", u.Items)
	}
}

func TestUndeliveredOverflowCounted(t *testing.T) {
	var u Undelivered
	for i := 1; i <= MaxUndelivered+3; i++ {
		u.Add(item(i))
	}

	if len(u.Items) != MaxUndelivered || u.Folded != 3 {
		t.Fatalf("len %d folded %d, want %d and 3", len(u.Items), u.Folded, MaxUndelivered)
	}
	if u.Items[0].Key != item(1).Key || u.Items[MaxUndelivered-1].Key != item(MaxUndelivered).Key {
		t.Fatal("items must be kept oldest first")
	}

	header := "Undelivered earlier:\n"
	counted := "… and 3 more undelivered PROBLEMs\n"
	line1 := "PROBLEM: job01 failed (since 10:01), no longer open\n"

	// The counted line's space is reserved before the item lines: with room
	// for one line plus the counted line, one line and the counted line show.
	limit := utf16Len(header + line1 + counted + "\n")
	text, shown := u.Prefix(limit, chanT0())
	if want := header + line1 + counted + "\n"; text != want {
		t.Fatalf("Prefix =\n%q\nwant\n%q", text, want)
	}
	if shown != (Shown{Items: 1, Folded: 3}) {
		t.Fatalf("shown = %+v", shown)
	}

	// Room for no item line: the counted line is still shown.
	text, shown = u.Prefix(utf16Len(header+counted+"\n"), chanT0())
	if want := header + counted + "\n"; text != want {
		t.Fatalf("no-room Prefix = %q, want %q", text, want)
	}
	if shown != (Shown{Folded: 3}) {
		t.Fatalf("no-room shown = %+v", shown)
	}

	// Ack resets Folded and frees room; a fold counted after the Prefix is kept.
	_, shown = u.Prefix(limit, chanT0())
	u.Add(item(99))
	u.Ack(shown)
	if len(u.Items) != MaxUndelivered-1 || u.Folded != 1 {
		t.Fatalf("after Ack: len %d folded %d, want %d and 1", len(u.Items), u.Folded, MaxUndelivered-1)
	}

	u.Ack(Shown{Folded: 1})
	u.Add(item(100))
	if len(u.Items) != MaxUndelivered || u.Folded != 0 {
		t.Fatalf("freed slot not reused: len %d folded %d", len(u.Items), u.Folded)
	}
}

func TestUndeliveredCodec(t *testing.T) {
	empty, err := DecodeUndelivered("")
	if err != nil || len(empty.Items) != 0 || empty.Folded != 0 {
		t.Fatalf("DecodeUndelivered(\"\") = %+v, %v", empty, err)
	}

	var u Undelivered
	u.Add(item(1))
	u.Folded = 7

	s, err := u.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	back, err := DecodeUndelivered(s)
	if err != nil {
		t.Fatalf("DecodeUndelivered: %v", err)
	}
	if back.Folded != 7 || len(back.Items) != 1 {
		t.Fatalf("round trip = %+v", back)
	}
	got, want := back.Items[0], item(1)
	if got.Key != want.Key || got.Text != want.Text || !got.Since.Equal(want.Since) ||
		!got.DroppedAt.Equal(want.DroppedAt) {
		t.Fatalf("item round trip = %+v, want %+v", got, want)
	}

	// A stored list over the cap folds its newest excess instead of losing it.
	var big Undelivered
	for i := range MaxUndelivered {
		big.Add(item(i))
	}
	big.Items = append(big.Items, item(98), item(99))
	big.Folded = 1
	s, err = big.Encode()
	if err != nil {
		t.Fatalf("Encode big: %v", err)
	}
	back, err = DecodeUndelivered(s)
	if err != nil {
		t.Fatalf("DecodeUndelivered big: %v", err)
	}
	if len(back.Items) != MaxUndelivered || back.Folded != 3 {
		t.Fatalf("over-cap decode: len %d folded %d", len(back.Items), back.Folded)
	}

	if _, err := DecodeUndelivered("[1"); err == nil {
		t.Fatal("malformed list decoded without error")
	}

	if !strings.Contains(s, `"items"`) || !strings.Contains(s, `"folded"`) {
		t.Fatalf("encoded form %s lacks items/folded fields", s)
	}
}

// exportState returns the three kv values of n's delivery state.
func exportState(t *testing.T, n *Notifier) (channels, sms, undelivered string) {
	t.Helper()

	var err error
	if channels, err = n.ExportChannels(); err != nil {
		t.Fatal(err)
	}
	if sms, err = n.ExportSMSProblems(); err != nil {
		t.Fatal(err)
	}
	if undelivered, err = n.ExportUndelivered(); err != nil {
		t.Fatal(err)
	}

	return channels, sms, undelivered
}

// restoreState hands the kv values to n.
func restoreState(t *testing.T, n *Notifier, channels, sms, undelivered string) {
	t.Helper()

	if err := n.RestoreChannels(channels); err != nil {
		t.Fatal(err)
	}
	if err := n.RestoreSMSProblems(sms); err != nil {
		t.Fatal(err)
	}
	if err := n.RestoreUndelivered(undelivered); err != nil {
		t.Fatal(err)
	}
}

// TestDeliveryStateSurvivesARestart: the Telegram failure and its notice,
// the SMS-sent set and the undelivered list, with its Folded count, are
// exported as kv values and restored into a fresh notifier.
func TestDeliveryStateSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	before := newClockFixture(ModeTelegram)
	before.tlg.fail = true
	before.send(t, Message{Kind: KindProblem, Check: "ups", Key: "ups/main", Text: "on battery"})
	failing := before.n.TelegramHealth().FailingSince
	before.n.MarkTelegramReported(failing)
	for i := range MaxUndelivered + 2 {
		before.n.ListUndelivered(QueuedMessage{Key: fmt.Sprintf("k%d", i), Plain: "lost"})
	}
	if len(before.sms.sent) != 1 {
		t.Fatalf("sms = %q, want the PROBLEM sent by the fallback", before.sms.sent)
	}

	after := newClockFixture(ModeTelegram)
	channels, sms, undelivered := exportState(t, before.n)
	restoreState(t, after.n, channels, sms, undelivered)

	if h := after.n.TelegramHealth(); !h.FailingSince.Equal(failing) || !h.Reported {
		t.Fatalf("telegram = %+v, want failing since %v and reported", h, failing)
	}
	if !after.n.smsProblems.Has("ups/main") {
		t.Fatal("SMS-sent set lost ups/main")
	}
	if u := after.n.undelivered; len(u.Items) != MaxUndelivered || u.Folded != 2 {
		t.Fatalf("undelivered = %d items, %d folded; want %d and 2", len(u.Items), u.Folded, MaxUndelivered)
	}

	// The restored set keeps the fallback from sending the PROBLEM again,
	// and its RESOLVED goes by SMS.
	after.tlg.fail = true
	after.n.Flush(ctx)
	after.send(t, Message{Kind: KindResolved, Check: "ups", Key: "ups/main", Text: "on battery"})
	if want := []string{"host1.example.org: RESOLVED: on battery"}; !slices.Equal(after.sms.sent, want) {
		t.Fatalf("sms after restart = %q, want %q", after.sms.sent, want)
	}
}

// TestRestoreChannelsMergesWithMemory: state loaded late joins what memory
// saw meanwhile. A success seen in memory wins (Telegram works); otherwise a
// stored failure is the same outage: the earlier since, reported when either
// side reported it.
func TestRestoreChannelsMergesWithMemory(t *testing.T) {
	early, late := chanT0(), chanT0().Add(time.Hour)
	tests := []struct {
		name   string
		memory func(n *Notifier)
		stored ChannelHealth
		want   ChannelHealth
	}{
		{
			name:   "nothing seen: stored taken",
			memory: func(*Notifier) {},
			stored: ChannelHealth{FailingSince: early, Reported: true},
			want:   ChannelHealth{FailingSince: early, Reported: true},
		},
		{
			name:   "success seen: memory wins",
			memory: func(n *Notifier) { n.telegramSentLocked(nil) },
			stored: ChannelHealth{FailingSince: early, Reported: true},
			want:   ChannelHealth{},
		},
		{
			name: "success then failure seen: memory wins",
			memory: func(n *Notifier) {
				n.telegramSentLocked(nil)
				n.telegram = ChannelHealth{FailingSince: late}
			},
			stored: ChannelHealth{FailingSince: early, Reported: true},
			want:   ChannelHealth{FailingSince: late},
		},
		{
			name:   "failing in memory: the earlier since, reported",
			memory: func(n *Notifier) { n.telegram = ChannelHealth{FailingSince: late} },
			stored: ChannelHealth{FailingSince: early, Reported: true},
			want:   ChannelHealth{FailingSince: early, Reported: true},
		},
		{
			name:   "reported in memory: kept",
			memory: func(n *Notifier) { n.telegram = ChannelHealth{FailingSince: early, Reported: true} },
			stored: ChannelHealth{FailingSince: late},
			want:   ChannelHealth{FailingSince: early, Reported: true},
		},
		{
			name:   "stored OK: memory kept",
			memory: func(n *Notifier) { n.telegram = ChannelHealth{FailingSince: late} },
			stored: ChannelHealth{},
			want:   ChannelHealth{FailingSince: late},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newClockFixture(ModeTelegram).n
			tt.memory(n)
			v, err := Channels{ChannelTelegram: tt.stored}.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if err := n.RestoreChannels(v); err != nil {
				t.Fatal(err)
			}
			if got := n.TelegramHealth(); !got.FailingSince.Equal(tt.want.FailingSince) ||
				got.Reported != tt.want.Reported {
				t.Fatalf("telegram = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestRestoreSMSProblemsSkipsKeysResolvedSinceStart: a stored key whose
// RESOLVED was seen in memory before the late load is not put back, so its
// next RESOLVED does not go by SMS again.
func TestRestoreSMSProblemsSkipsKeysResolvedSinceStart(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.n.smsProblems.Add("memory/c")
	cf.send(t, Message{Kind: KindResolved, Key: "resolved/b", Text: "b ok"})

	if err := cf.n.RestoreSMSProblems(`["stored/a","resolved/b"]`); err != nil {
		t.Fatal(err)
	}
	if !cf.n.smsProblems.Has("stored/a") || !cf.n.smsProblems.Has("memory/c") ||
		cf.n.smsProblems.Has("resolved/b") {
		t.Fatalf("set = %v, want stored/a and memory/c only", cf.n.smsProblems.keys)
	}
}

// TestRestoreUndeliveredMergesWithMemory: stored items, older, go ahead of
// those listed in memory, Folded counts add up and items over the capacity
// are counted; while a send holds the prefix, stored items go after the
// items it shows, so its acknowledgement removes exactly those.
func TestRestoreUndeliveredMergesWithMemory(t *testing.T) {
	stored := func(u Undelivered) string {
		t.Helper()
		v, err := u.Encode()
		if err != nil {
			t.Fatal(err)
		}

		return v
	}
	keys := func(u Undelivered) []string {
		out := make([]string, 0, len(u.Items))
		for _, it := range u.Items {
			out = append(out, it.Key)
		}

		return out
	}

	t.Run("stored first", func(t *testing.T) {
		n := newClockFixture(ModeTelegram).n
		n.undelivered = Undelivered{Items: []UndeliveredProblem{item(9)}, Folded: 1}
		older := Undelivered{Items: []UndeliveredProblem{item(1), item(2)}, Folded: 3}
		if err := n.RestoreUndelivered(stored(older)); err != nil {
			t.Fatal(err)
		}
		want := []string{"command/job01", "command/job02", "command/job09"}
		if got := keys(n.undelivered); !slices.Equal(got, want) || n.undelivered.Folded != 4 {
			t.Fatalf("items = %v, folded %d; want %v, 4", got, n.undelivered.Folded, want)
		}
	})

	t.Run("over capacity counted", func(t *testing.T) {
		n := newClockFixture(ModeTelegram).n
		n.undelivered = Undelivered{Items: []UndeliveredProblem{item(99)}}
		var full Undelivered
		for i := range MaxUndelivered {
			full.Add(item(i))
		}
		if err := n.RestoreUndelivered(stored(full)); err != nil {
			t.Fatal(err)
		}
		if len(n.undelivered.Items) != MaxUndelivered || n.undelivered.Folded != 1 ||
			n.undelivered.Items[0].Key != "command/job00" {
			t.Fatalf("items = %d, folded %d; want the stored %d and one counted",
				len(n.undelivered.Items), n.undelivered.Folded, MaxUndelivered)
		}
	})

	t.Run("prefix held", func(t *testing.T) {
		n := newClockFixture(ModeTelegram).n
		n.undelivered = Undelivered{Items: []UndeliveredProblem{item(8), item(9)}}
		now := n.opts.Now()
		limit := utf16Len(undeliveredHeader+"\n") + utf16Len(undeliveredLine(item(8), now)) + utf16Len("\n")
		if _, shown, held := n.takePrefix(limit); !held || shown.Items != 1 {
			t.Fatalf("prefix held %v showing %+v, want one item", held, shown)
		}
		if err := n.RestoreUndelivered(stored(Undelivered{Items: []UndeliveredProblem{item(1)}})); err != nil {
			t.Fatal(err)
		}
		n.releasePrefix(Shown{Items: 1}, true)
		if got, want := keys(n.undelivered), []string{"command/job01", "command/job09"}; !slices.Equal(got, want) {
			t.Fatalf("items = %v, want %v", got, want)
		}
	})
}

// TestRestoreDeliveryStateRejectsCorruptValues: a value that does not decode
// is an error and leaves memory as it was.
func TestRestoreDeliveryStateRejectsCorruptValues(t *testing.T) {
	cf := newClockFixture(ModeTelegram)
	cf.n.telegram = ChannelHealth{FailingSince: chanT0()}
	cf.n.smsProblems.Add("k")
	cf.n.undelivered.Add(item(1))

	for name, restore := range map[string]func(string) error{
		"channels": cf.n.RestoreChannels, "sms": cf.n.RestoreSMSProblems, "undelivered": cf.n.RestoreUndelivered,
	} {
		if err := restore("{"); err == nil {
			t.Fatalf("%s: corrupt value restored without error", name)
		}
	}
	if !cf.n.telegram.FailingSince.Equal(chanT0()) || !cf.n.smsProblems.Has("k") || len(cf.n.undelivered.Items) != 1 {
		t.Fatalf("memory changed: %+v, %v, %+v", cf.n.telegram, cf.n.smsProblems.keys, cf.n.undelivered)
	}
}

// TestUndeliveredFromRestoredSinceInHostZone: a since read back from the
// store (UTC) is listed in the host's zone.
func TestUndeliveredFromRestoredSinceInHostZone(t *testing.T) {
	loc := time.FixedZone("TST", 2*60*60)
	item := QueuedMessage{
		Kind: string(KindProblem), Key: "k", Plain: "hot",
		Since: time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC), CreatedAt: time.Date(2026, 1, 2, 8, 30, 0, 0, time.UTC),
	}
	got := undeliveredFrom(item, "h", loc, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC))
	if got.Since.Location() != loc {
		t.Fatalf("since = %v, want it in the host's zone", got.Since)
	}
	line := undeliveredLine(got, time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC))
	if line != "PROBLEM: hot (since 10:00), no longer open\n" {
		t.Fatalf("line = %q", line)
	}
}
