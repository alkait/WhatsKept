package app

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// collect runs pumpLines against the given input and returns the
// emitted line slice plus the terminal error.
func collect(t *testing.T, input string) ([]string, error) {
	t.Helper()
	var got []string
	err := pumpLines(strings.NewReader(input), 1024*1024, func(s string) {
		got = append(got, s)
	})
	return got, err
}

// TestPumpLines_ProgressStreamDropsRedraws verifies that a stream
// consisting of many '\r'-separated progress redraws (as emitted by
// idevicebackup2 during the receive phase) does NOT flood the line
// emitter. Each '\r'-terminated segment is consumed off the pipe but
// not surfaced as a line event — only newline-terminated lines are.
// This both prevents the original "token too long" stall (the pipe
// is drained continuously) and the UI freeze that would result from
// thousands of per-redraw SSE events.
func TestPumpLines_ProgressStreamDropsRedraws(t *testing.T) {
	var b strings.Builder
	// 20000 progress redraws ≈ 1.6 MB, all separated by '\r' with
	// no '\n' until the final terminal line.
	for i := 0; i < 20000; i++ {
		b.WriteString("[================================================  ] 97% (53.3 MB/54.1 MB)")
		b.WriteByte('\r')
	}
	b.WriteString("Receiving files\n")

	got, err := collect(t, b.String())
	if err != nil {
		t.Fatalf("pumpLines error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 emitted line, got %d (first=%q)", len(got), firstOr(got, ""))
	}
	if got[0] != "Receiving files" {
		t.Fatalf("unexpected emitted line: %q", got[0])
	}
}

// TestPumpLines_MixedEndings covers handling of '\n', bare '\r' and
// CRLF interleaved in one stream:
//   - '\n' emits whatever was accumulated.
//   - bare '\r' silently discards the accumulator (progress redraw).
//   - CRLF emits once with the trailing '\r' stripped.
func TestPumpLines_MixedEndings(t *testing.T) {
	got, err := collect(t, "alpha\nbeta\rgamma\r\ndelta\n")
	if err != nil {
		t.Fatalf("pumpLines error: %v", err)
	}
	want := []string{"alpha", "gamma", "delta"}
	assertLines(t, got, want)
}

// TestPumpLines_TrailingDataAtEOF makes sure that a final line
// without a trailing newline is still emitted at EOF.
func TestPumpLines_TrailingDataAtEOF(t *testing.T) {
	got, err := collect(t, "one\ntwo")
	if err != nil {
		t.Fatalf("pumpLines error: %v", err)
	}
	assertLines(t, got, []string{"one", "two"})
}

// TestPumpLines_OverlongLineTruncated guards the OOM safety: a line
// with no terminator longer than maxLine is silently truncated but
// the read loop still drains the pipe and emits the truncated line
// at EOF (here, an empty line because every byte was dropped after
// the 4-byte cap was reached on the first 4 bytes of "aaaa...").
func TestPumpLines_OverlongLineTruncated(t *testing.T) {
	input := strings.Repeat("a", 10000) + "\nshort\n"
	var got []string
	err := pumpLines(strings.NewReader(input), 4, func(s string) {
		got = append(got, s)
	})
	if err != nil {
		t.Fatalf("pumpLines error: %v", err)
	}
	assertLines(t, got, []string{"aaaa", "shor"})
}

// TestPumpLines_PropagatesReadError makes sure that non-EOF read
// errors surface to the caller (so pumpProcess can log them and
// drain defensively) AND that any partial accumulator is still
// flushed as a final line so partial output isn't silently lost
// when, e.g., the child process is killed mid-line.
func TestPumpLines_PropagatesReadError(t *testing.T) {
	boom := errors.New("boom")
	r := &errReader{data: []byte("partial"), err: boom}
	var got []string
	err := pumpLines(r, 1024, func(s string) { got = append(got, s) })
	if !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	assertLines(t, got, []string{"partial"})
}

// errReader yields `data` then returns `err` (which is intentionally
// not io.EOF, to exercise the non-EOF error path of pumpLines).
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, io.EOF
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d lines %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func firstOr(s []string, dflt string) string {
	if len(s) == 0 {
		return dflt
	}
	return s[0]
}
