package tools

import (
	"encoding/json"
	"testing"

	"github.com/synthlike/whiff/internal/puff"
)

// TestArg reads a tool's arguments as puff, then a JSON array or object
// as a model writes one, then as a string.
func TestArg(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`["go" "test" "./..."]`, `["go" "test" "./..."]`},
		{`["go","test","./..."]`, `["go" "test" "./..."]`},
		{` {"turn": 1, "day": 2.5} `, `{:day 2.5 :turn 1}`},
		{`1d`, `1d`},
		{`"quoted"`, `"quoted"`},
		{`plain words`, `"plain words"`},
		{`[1, 2`, `"[1, 2"`},
	} {
		if got := puff.Print(Arg(tt.in)); got != tt.want {
			t.Errorf("Arg(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

// TestArgs takes a call's arguments in its params' order, up to the first
// left out; a bare number or object, as models other than Claude write
// them, reads as JSON.
func TestArgs(t *testing.T) {
	f := Fn{Params: []string{"text", "n", "opts"}}
	for _, tt := range []struct{ in, want string }{
		{`{"text": "\"milk\"", "n": "2"}`, `["milk" 2]`},
		{`{"text": "milk", "n": 2, "opts": {"x": 1}}`, `["milk" 2 {:x 1}]`},
		{`{"n": "2"}`, `[]`},
		{``, `[]`},
	} {
		v, err := Args(f, json.RawMessage(tt.in))
		if err != nil || puff.Print(v) != tt.want {
			t.Errorf("Args(%s) = %s %v, want %s", tt.in, puff.Print(v), err, tt.want)
		}
	}
	if _, err := Args(f, json.RawMessage(`[1]`)); err == nil {
		t.Error("an array read as arguments")
	}
}

// TestResult reads a call's answer as text for the model: an ok's value,
// a pending id with what to do, an err as an error.
func TestResult(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		isErr    bool
	}{
		{`(ok "hi")`, `hi`, false},
		{`(ok [1 2])`, `[1 2]`, false},
		{`(err {:kind :pending :id 7 :message "held for review"})`, `pending 7: held for review; go on`, false},
		{`(err {:kind :nope})`, `{:kind :nope}`, true},
		{`42`, `42`, false},
	} {
		if text, isErr := Result(tt.in, "go on"); text != tt.want || isErr != tt.isErr {
			t.Errorf("Result(%s) = %q %v", tt.in, text, isErr)
		}
	}
}
