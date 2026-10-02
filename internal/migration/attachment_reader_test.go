package migration

import (
	"errors"
	"strings"
	"testing"
)

func TestAttachmentReaderDefaultsToPlainRead(t *testing.T) {
	sudoCalled := false
	asUser := func(string) ([]byte, error) { return []byte("plain"), nil }
	withSudo := func(string) ([]byte, error) { sudoCalled = true; return []byte("sudo"), nil }

	data, err := attachmentReader(asUser, withSudo, false)("/data/a.png")
	if err != nil || string(data) != "plain" {
		t.Fatalf("got %q, %v; want plain read", data, err)
	}
	if sudoCalled {
		t.Error("sudo read used without read_with_sudo")
	}
}

func TestAttachmentReaderPlainFailureNamesOption(t *testing.T) {
	asUser := func(string) ([]byte, error) { return nil, errors.New("Permission denied") }
	withSudo := func(string) ([]byte, error) { t.Error("sudo read used"); return nil, nil }

	_, err := attachmentReader(asUser, withSudo, false)("/data/a.png")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "mattermost.files.read_with_sudo") {
		t.Errorf("error should keep the cause and name read_with_sudo: %v", err)
	}
}

func TestAttachmentReaderWithSudoOption(t *testing.T) {
	asUser := func(string) ([]byte, error) { t.Error("plain read used"); return nil, nil }
	withSudo := func(string) ([]byte, error) { return []byte("sudo"), nil }

	data, err := attachmentReader(asUser, withSudo, true)("/data/a.png")
	if err != nil || string(data) != "sudo" {
		t.Fatalf("got %q, %v; want sudo read", data, err)
	}
}
