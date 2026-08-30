package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("warn", "json", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Debug("debug msg")
	log.Info("info msg")
	log.Warn("warn msg")
	out := buf.String()
	if strings.Contains(out, "info msg") || strings.Contains(out, "debug msg") {
		t.Errorf("levels below warn should be filtered, got: %s", out)
	}
	if !strings.Contains(out, "warn msg") {
		t.Errorf("warn should be logged, got: %s", out)
	}
}

func TestNewFormats(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("info", "text", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("text format expected, got: %s", buf.String())
	}

	if _, err := New("info", "yaml", &buf); err == nil {
		t.Error("unknown format should error")
	}
	if _, err := New("loud", "json", &buf); err == nil {
		t.Error("unknown level should error")
	}
}

func TestComponent(t *testing.T) {
	var buf bytes.Buffer
	log, err := New("info", "json", &buf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	Component(log, "mdns").Info("hello")
	if !strings.Contains(buf.String(), `"logger":"mdns"`) {
		t.Errorf("component name missing, got: %s", buf.String())
	}
}
