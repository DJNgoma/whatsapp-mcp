package main

import (
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestExtractDirectPathPreservesSignedQuery(t *testing.T) {
	rawURL := "https://mmg.whatsapp.net/v/t62/example.enc?ccb=11-4&oh=signed&oe=expiry"
	want := "/v/t62/example.enc?ccb=11-4&oh=signed&oe=expiry"
	if got := extractDirectPathFromURL(rawURL); got != want {
		t.Fatalf("extractDirectPathFromURL() = %q, want %q", got, want)
	}
}

func TestDefaultBridgePort(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_PORT", "9123")
	if got := defaultBridgePort(); got != 9123 {
		t.Fatalf("defaultBridgePort() = %d, want 9123", got)
	}
}

func TestDefaultBridgePortRejectsInvalidValue(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_PORT", "not-a-port")
	if got := defaultBridgePort(); got != 8741 {
		t.Fatalf("defaultBridgePort() = %d, want 8741", got)
	}
}

func TestDefaultStoreDir(t *testing.T) {
	t.Setenv("WHATSAPP_STORE_DIR", "/tmp/whatsapp-mcp-test-store")
	if got := defaultStoreDir(); got != "/tmp/whatsapp-mcp-test-store" {
		t.Fatalf("defaultStoreDir() = %q", got)
	}
}

func TestPortableBaseName(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{`C:\Users\person\Documents\invoice.pdf`, "invoice.pdf"},
		{"/home/person/Documents/invoice.pdf", "invoice.pdf"},
		{"invoice.pdf", "invoice.pdf"},
	} {
		if got := portableBaseName(test.path); got != test.want {
			t.Errorf("portableBaseName(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestPDFDocumentMetadata(t *testing.T) {
	mediaType, mimeType := mediaTypeAndMIME(`C:\Users\person\Documents\Price List.pdf`)
	if mediaType != whatsmeow.MediaDocument {
		t.Fatalf("mediaTypeAndMIME() media type = %q, want document", mediaType)
	}
	if mimeType != "application/pdf" {
		t.Fatalf("mediaTypeAndMIME() MIME type = %q, want application/pdf", mimeType)
	}

	document := newDocumentMessage(
		`C:\Users\person\Documents\Price List.pdf`,
		"Latest price list",
		mimeType,
		whatsmeow.UploadResponse{URL: "https://example.invalid/file", FileLength: 1234},
	)
	if document.GetFileName() != "Price List.pdf" {
		t.Errorf("DocumentMessage.FileName = %q, want Price List.pdf", document.GetFileName())
	}
	if document.GetTitle() != "Price List.pdf" {
		t.Errorf("DocumentMessage.Title = %q, want Price List.pdf", document.GetTitle())
	}
	if document.GetMimetype() != "application/pdf" {
		t.Errorf("DocumentMessage.Mimetype = %q, want application/pdf", document.GetMimetype())
	}
}

func TestPairingIdentityMatchesHostPlatform(t *testing.T) {
	tests := map[string]string{
		"darwin":  "macOS",
		"linux":   "Linux",
		"windows": "Windows",
	}
	for goos, want := range tests {
		if got := identityForOS(goos).osName; got != want {
			t.Errorf("identityForOS(%q).osName = %q, want %q", goos, got, want)
		}
	}
}

func TestImageOpenCommandMatchesHostPlatform(t *testing.T) {
	tests := map[string]string{
		"darwin":  "open",
		"linux":   "xdg-open",
		"windows": "rundll32",
	}
	for goos, want := range tests {
		command, _, err := imageOpenCommand(goos, "pairing.png")
		if err != nil {
			t.Fatalf("imageOpenCommand(%q) returned error: %v", goos, err)
		}
		if command != want {
			t.Errorf("imageOpenCommand(%q) = %q, want %q", goos, command, want)
		}
	}
}
