package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"knot-core/pkg/core"
)

func TestVersionWithoutStartup(t *testing.T) {
	t.Setenv("KNOT_TEST_CORE_PROCESS", "1")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--version")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "knot-core "+core.DefaultVersion+"\n" {
		t.Fatalf("--version: %q, %v", output, err)
	}
	files, err := os.ReadDir(cmd.Dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("--version changed the working directory: %v, %v", files, err)
	}
}

func TestHelpWithoutStartup(t *testing.T) {
	t.Setenv("KNOT_TEST_CORE_PROCESS", "1")
	t.Setenv("LANGUAGE", "")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, locale := range []string{"C", "zh_CN.UTF-8"} {
		t.Run(locale, func(t *testing.T) {
			t.Setenv("LC_ALL", locale)
			heading := "Usage:"
			if locale != "C" {
				heading = "用法："
			}
			dir := t.TempDir()
			for _, tc := range []struct {
				args   []string
				syntax string
			}{
				{[]string{"--help"}, "[--config <file>]"},
				{[]string{"-h"}, "[--config <file>]"},
				{[]string{"help"}, "[--config <file>]"},
				{[]string{"--config", "missing.toml", "--help"}, "[--config <file>]"},
				{[]string{"init", "--help"}, "init"},
				{[]string{"init", "-h"}, "init"},
				{[]string{"help", "init"}, "init"},
				{[]string{"help", "--help"}, "help [command]"},
			} {
				cmd := exec.Command(binary, tc.args...)
				cmd.Dir = dir
				var diagnostics bytes.Buffer
				cmd.Stderr = &diagnostics
				output, err := cmd.Output()
				if err != nil || diagnostics.Len() > 0 {
					t.Fatalf("%q: %v, stderr=%q", tc.args, err, diagnostics.String())
				}
				if !strings.HasPrefix(string(output), heading+"\n  knot-core "+tc.syntax+"\n") {
					t.Fatalf("%q: unexpected help %q", tc.args, output)
				}
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 {
				t.Fatalf("help changed the working directory: %v, %v", files, err)
			}
		})
	}
}
