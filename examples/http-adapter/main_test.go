package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRunAdapterFailsClosedBeforeWritingProtocolOutput(t *testing.T) {
	var stdout, stderr synchronizedBuffer
	err := runAdapter(context.Background(), "", strings.NewReader(""), &stdout, &stderr)
	if err == nil || stdout.String() != "" {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	path := writeManifest(t, map[string]any{"version": "2", "operations": []any{}})
	err = runAdapter(context.Background(), path, strings.NewReader(""), &stdout, &stderr)
	if err == nil || stdout.String() != "" {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestRunAdapterLoadsManifestAndKeepsStdoutProtocolOnly(t *testing.T) {
	path := writeManifest(t, validManifestMap())
	provider := map[string]any{"name": "public-api", "endpoint": "https://api.example"}
	input := requestLine("discover-1", "discover", map[string]any{"provider": provider})
	var stdout, stderr synchronizedBuffer
	if err := runAdapter(context.Background(), path, strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, stdout.String())
	if len(responses) != 1 || responses[0].Error != nil {
		t.Fatalf("responses=%#v", responses)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if err := os.WriteFile(path, []byte(`{"version":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 1 {
		t.Fatal("completed protocol response changed after manifest file mutation")
	}
}

func TestRunAdapterPropagatesUnterminatedProtocolError(t *testing.T) {
	path := writeManifest(t, validManifestMap())
	provider := map[string]any{"name": "public-api", "endpoint": "https://api.example"}
	input := strings.TrimSuffix(requestLine("unterminated", "discover", map[string]any{"provider": provider}), "\n")
	var stdout, stderr synchronizedBuffer
	err := runAdapter(context.Background(), path, strings.NewReader(input), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "newline") || stdout.String() != "" {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}
