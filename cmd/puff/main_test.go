package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	file := filepath.Join(t.TempDir(), "t.puff")
	if err := os.WriteFile(file, []byte("(def x ; the answer\n  42)\n"), 0o644); err != nil {
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
			args:    []string{"-e", "(ok [1 {:b 2 :a 1}]) 60s"},
			wantOut: "(ok [1 {:b 2 :a 1}])\n1m\n",
		},
		{
			name:    "-e error",
			args:    []string{"-e", "(a"},
			code:    1,
			wantErr: "-e:1:1: no closing ')'\n",
		},
		{
			name:    "file",
			args:    []string{file},
			wantOut: "(def x 42)\n",
		},
		{
			name:    "missing file",
			args:    []string{"nope.puff"},
			code:    1,
			wantErr: "puff: open nope.puff: no such file or directory\n",
		},
		{
			name:    "stdin",
			stdin:   "(a\n b) :c\n(x ]\n\"one\ntwo\"\n[1\n",
			wantOut: "(a b)\n:c\n\"one\\ntwo\"\n",
			wantErr: "1:4: unexpected ']'\n1:1: no closing ']'\n",
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
