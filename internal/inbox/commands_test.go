package inbox

import (
	"errors"
	"testing"
	"time"
)

func TestParseRecognisesCommands(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain mute", "mute 2h", "mute"},
		{"slashed mute", "/mute 2h", "mute"},
		{"slashed with bot suffix", "/mute@RaedarBot 2h", "mute"},
		{"plain ack", "ack 12", "ack"},
		{"plain status", "status", "status"},
		{"plain help", "help", "help"},
		{"plain unmute", "unmute 4", "unmute"},
		{"leading whitespace", "   ack 12", "ack"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.in, err)
			}
			if cmd.Name != tc.want {
				t.Fatalf("Parse(%q).Name = %q, want %q", tc.in, cmd.Name, tc.want)
			}
		})
	}
}

func TestParseRejectsNonCommands(t *testing.T) {
	for _, in := range []string{"", "   ", "hello there", "acknowledge this", "what can you do"} {
		_, err := Parse(in)
		if !errors.Is(err, ErrUnknownCmd) {
			t.Fatalf("Parse(%q) err = %v, want ErrUnknownCmd", in, err)
		}
	}
}

// now is the fixed clock the mute parsing tests use.
var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestParseMuteArgsSilent(t *testing.T) {
	_, _, endsAt, err := ParseMuteArgs([]string{"2h"}, now)
	if err != nil {
		t.Fatalf("ParseMuteArgs error = %v", err)
	}
	want := now.Add(2 * time.Hour)
	if !endsAt.Equal(want) {
		t.Fatalf("endsAt = %v, want %v", endsAt, want)
	}
}

func TestParseMuteArgsSilentUntil(t *testing.T) {
	_, _, endsAt, err := ParseMuteArgs([]string{"until", "2026-10-21T08:00:00Z"}, now)
	if err != nil {
		t.Fatalf("ParseMuteArgs error = %v", err)
	}
	want := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	if !endsAt.Equal(want) {
		t.Fatalf("endsAt = %v, want %v", endsAt, want)
	}
}

func TestParseMuteArgsFull(t *testing.T) {
	rule, subject, endsAt, err := ParseMuteArgs([]string{"route.down", "fallback@file", "30m"}, now)
	if err != nil {
		t.Fatalf("ParseMuteArgs error = %v", err)
	}
	if rule != "route.down" || subject != "fallback@file" {
		t.Fatalf("got rule=%q subject=%q", rule, subject)
	}
	if want := now.Add(30 * time.Minute); !endsAt.Equal(want) {
		t.Fatalf("endsAt = %v, want %v", endsAt, want)
	}
}

func TestParseMuteArgsFullUntil(t *testing.T) {
	rule, subject, endsAt, err := ParseMuteArgs([]string{"route.down", "fallback@file", "until", "2026-10-21T08:00:00Z"}, now)
	if err != nil {
		t.Fatalf("ParseMuteArgs error = %v", err)
	}
	if rule != "route.down" || subject != "fallback@file" {
		t.Fatalf("got rule=%q subject=%q", rule, subject)
	}
	if endsAt.Format(time.RFC3339) != "2026-10-21T08:00:00Z" {
		t.Fatalf("endsAt = %v", endsAt)
	}
}

func TestParseMuteArgsErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"only-rule"},
		{"route.down"},
		{"route.down", "fallback@file"},
		{"route.down", "fallback@file", "until"},
		{"route.down", "fallback@file", "until", "not-a-time"},
		{"route.down", "fallback@file", "banana"},
		{"route.down", "fallback@file", "-5m"},
		{"route.down", "fallback@file", "30m", "extra"},
		{"until"},
		{"until", "not-a-time"},
		{"not-a-duration"},
		{"-1h"},
	}
	for _, args := range cases {
		_, _, _, err := ParseMuteArgs(args, now)
		if err == nil {
			t.Fatalf("ParseMuteArgs(%v) = nil err, want error", args)
		}
	}
}

func TestParseAckArgs(t *testing.T) {
	id, err := ParseAckArgs([]string{"42"})
	if err != nil {
		t.Fatalf("ParseAckArgs error = %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d", id)
	}
	if _, err := ParseAckArgs([]string{}); err == nil {
		t.Fatal("empty args: want error")
	}
	if _, err := ParseAckArgs([]string{"abc"}); err == nil {
		t.Fatal("non-numeric: want error")
	}
	if _, err := ParseAckArgs([]string{"1", "2"}); err == nil {
		t.Fatal("too many args: want error")
	}
}

func TestParseUnmuteArgs(t *testing.T) {
	id, err := ParseUnmuteArgs([]string{"7"})
	if err != nil {
		t.Fatalf("ParseUnmuteArgs error = %v", err)
	}
	if id != 7 {
		t.Fatalf("id = %d", id)
	}
}
