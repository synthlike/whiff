package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "t.puff")
	if err := os.WriteFile(file, []byte("(def x ; the answer\n  42)\n(+ x 1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	faulty := filepath.Join(dir, "fault.puff")
	if err := os.WriteFile(faulty, []byte("(def x 1)\n(+ x 1.0)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name             string
		args             []string
		stdin            string
		code             int
		wantOut, wantErr string
	}{
		{
			name:    "-e",
			args:    []string{"-e", "(def (sq n) (* n n)) [(sq 3) 60s {:b 2 :a 1}]"},
			wantOut: "[9 1m {:a 1 :b 2}]\n",
		},
		{
			name:    "-e read error",
			args:    []string{"-e", "(a"},
			code:    1,
			wantErr: "-e:1:1: no closing ')'\n",
		},
		{
			name:    "-e unknown name",
			args:    []string{"-e", "(nope)"},
			code:    1,
			wantErr: "-e:1:2: unknown name nope\n",
		},
		{
			name:    "-e fault",
			args:    []string{"-e", "(+ 1 1.0)"},
			code:    70,
			wantErr: "-e:1:1: +: can't add int and float\n",
		},
		{
			name: "file",
			args: []string{file},
		},
		{
			name:    "file fault",
			args:    []string{faulty},
			code:    70,
			wantErr: faulty + ":2:1: +: can't add int and float\n",
		},
		{
			name:    "missing file",
			args:    []string{"nope.puff"},
			code:    1,
			wantErr: "puff: open nope.puff: no such file or directory\n",
		},
		{
			name:    "stdin",
			stdin:   "(def x\n 2) (* x 3)\n(x ]\n\"one\ntwo\"\n(+ x 1.0)\n[1\n",
			wantOut: "6\n\"one\\ntwo\"\n",
			wantErr: "1:4: unexpected ']'\n1:1: +: can't add int and float\n1:1: no closing ']'\n",
		},
		{
			name:    "bad flag",
			args:    []string{"-x"},
			code:    2,
			wantErr: usage,
		},
		{
			name:    "-e without code",
			args:    []string{"-e"},
			code:    2,
			wantErr: usage,
		},
	}
	for _, tt := range tests {
		var out, errOut strings.Builder
		code := run(tt.args, strings.NewReader(tt.stdin), &out, &errOut, false)
		if code != tt.code || out.String() != tt.wantOut || errOut.String() != tt.wantErr {
			t.Errorf("%s: got %d, stdout %q, stderr %q; want %d, %q, %q",
				tt.name, code, out.String(), errOut.String(), tt.code, tt.wantOut, tt.wantErr)
		}
	}
}
