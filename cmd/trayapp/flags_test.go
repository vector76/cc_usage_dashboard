package main

import (
	"flag"
	"io"
	"testing"
)

func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("trayapp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// `trayapp -version` must be a plain switch: as a string flag it failed with
// "flag needs an argument", and -version=x was ignored and started the app.
func TestParseFlagsVersionIsASwitch(t *testing.T) {
	_, showVersion, err := parseFlags(newTestFlagSet(), []string{"-version"})
	if err != nil {
		t.Fatalf("parseFlags(-version): %v", err)
	}
	if !showVersion {
		t.Error("parseFlags(-version) did not request the version")
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	configPath, showVersion, err := parseFlags(newTestFlagSet(), nil)
	if err != nil {
		t.Fatalf("parseFlags(): %v", err)
	}
	if configPath != "" || showVersion {
		t.Errorf("parseFlags() = (%q, %v), want (\"\", false)", configPath, showVersion)
	}

	configPath, _, err = parseFlags(newTestFlagSet(), []string{"-config", "x.yaml"})
	if err != nil || configPath != "x.yaml" {
		t.Errorf("parseFlags(-config x.yaml) = (%q, %v), want x.yaml", configPath, err)
	}
}
