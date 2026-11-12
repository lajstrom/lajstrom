package testutil

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

// TestRPM checks the frame of the package RPM writes, which the rpm plugin's tests parse: the
// lead, the header at HeaderStart after the padded signature header, the payload a gzip of a
// cpio newc archive of the regular files, and the same bytes for the same options.
func TestRPM(t *testing.T) {
	b, info := RPM(RPMOptions{})
	again, _ := RPM(RPMOptions{})
	switch {
	case !bytes.Equal(b, again):
		t.Error("the same options gave other bytes")
	case !bytes.Equal(b[:4], []byte{0xed, 0xab, 0xee, 0xdb}) || b[79] != 5:
		t.Errorf("lead % x", b[:8])
	case info.HeaderStart%8 != 0 || !bytes.Equal(b[info.HeaderStart:info.HeaderStart+4], []byte{0x8e, 0xad, 0xe8, 0x01}):
		t.Errorf("header at %d: % x", info.HeaderStart, b[info.HeaderStart:info.HeaderStart+4])
	case !bytes.Equal(b[info.HeaderEnd:], info.Payload) || info.Size != int64(len(b)):
		t.Error("the payload is not after the header, or Size is not the length")
	case info.Filename != "hello-1.0.0-1.el9.x86_64.rpm" || info.EVR != "1.0.0-1.el9" || info.SourceRPM != "hello-1.0.0-1.el9.src.rpm":
		t.Errorf("Filename %q, EVR %q, SourceRPM %q", info.Filename, info.EVR, info.SourceRPM)
	}
	il := binary.BigEndian.Uint32(b[info.HeaderStart+8:])
	if tag := binary.BigEndian.Uint32(b[info.HeaderStart+16:]); tag != 63 || il < 20 {
		t.Errorf("the header's first entry is tag %d of %d, not the immutable region", tag, il)
	}
	checkRPMPayload(t, info)
}

// TestRPMSource checks the names of a source package with an epoch.
func TestRPMSource(t *testing.T) {
	_, src := RPM(RPMOptions{Source: true, Epoch: "1"})
	if src.Arch != "src" || src.Filename != "hello-1.0.0-1.el9.src.rpm" || src.SourceRPM != "" || src.EVR != "1:1.0.0-1.el9" {
		t.Errorf("source package: Arch %q, Filename %q, SourceRPM %q, EVR %q", src.Arch, src.Filename, src.SourceRPM, src.EVR)
	}
}

// checkRPMPayload checks that the payload of info is the gzip of a cpio newc archive of the
// regular files.
func checkRPMPayload(t *testing.T, info RPMInfo) {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(info.Payload))
	if err != nil {
		t.Fatal(err)
	}
	cpio, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(cpio)) != info.ArchiveSize || !strings.HasPrefix(string(cpio), "070701") ||
		!strings.Contains(string(cpio), "./usr/bin/hello\x00") || !strings.Contains(string(cpio), "TRAILER!!!\x00") {
		t.Errorf("payload of %d bytes is not the cpio of the files", len(cpio))
	}
}
