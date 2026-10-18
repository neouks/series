//go:build windows

package server

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestCustomCommandQuotingWindows(t *testing.T) {
	if os.Getenv("SERIES_QUOTE_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				_ = json.NewEncoder(os.Stdout).Encode(os.Args[i+1:])
				os.Exit(0)
			}
		}
		os.Exit(2)
	}

	values := []string{
		"plain",
		"with space",
		`double"quote`,
		"single'quote",
		`trailing\`,
		"& | < > ^ %PATH% !bang!",
		"line1\nline2",
	}
	for _, value := range values {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			program := "& " + shellQuoteFor("powershell", os.Args[0])
			command := renderCommandTemplate(program+" -test.run=TestCustomCommandQuotingWindows -- {value}", map[string]any{"value": value}, "powershell")
			cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
			cmd.Env = append(os.Environ(), "SERIES_QUOTE_HELPER=1")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("command failed: %v\n%s", err, out)
			}
			var got []string
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if want := []string{value}; !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip = %#v, want %#v", got, want)
			}
		})
	}
}
