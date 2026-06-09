package main

import (
	"bufio"
	"bytes"
	"net"
	"testing"
)

func TestRCONCodecRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRCON(&buf, 7, rconTypeExec, "save-all flush"); err != nil {
		t.Fatalf("writeRCON: %v", err)
	}
	id, typ, body, err := readRCON(&buf)
	if err != nil {
		t.Fatalf("readRCON: %v", err)
	}
	if id != 7 || typ != rconTypeExec || body != "save-all flush" {
		t.Errorf("round trip = (%d, %d, %q), want (7, %d, %q)", id, typ, body, rconTypeExec, "save-all flush")
	}
}

func TestRCONReadRejectsImplausibleLength(t *testing.T) {
	// length field claims 4 bytes — below the 10-byte minimum (id+type+2 nulls).
	bad := []byte{0x04, 0x00, 0x00, 0x00, 0, 0, 0, 0}
	if _, _, _, err := readRCON(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error on implausible packet length")
	}
}

// TestRCONExecAgainstFakeServer drives the client through auth + two commands
// against a minimal in-process RCON server, exercising the whole exchange.
func TestRCONExecAgainstFakeServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	const password = "s3cret"
	gotCmds := make(chan string, 4)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		br := bufio.NewReader(conn)

		// Auth packet.
		id, _, body, err := readRCON(br)
		if err != nil {
			return
		}
		respID := id
		if body != password {
			respID = rconAuthFailedID // signal bad password
		}
		_ = writeRCON(conn, respID, rconTypeExec, "")

		// Two command packets.
		for i := 0; i < 2; i++ {
			cid, _, cbody, err := readRCON(br)
			if err != nil {
				return
			}
			gotCmds <- cbody
			_ = writeRCON(conn, cid, rconTypeRespVal, "")
		}
	}()

	if err := rconExec(ln.Addr().String(), password, "save-off", "save-all flush"); err != nil {
		t.Fatalf("rconExec: %v", err)
	}
	if c := <-gotCmds; c != "save-off" {
		t.Errorf("first command = %q, want save-off", c)
	}
	if c := <-gotCmds; c != "save-all flush" {
		t.Errorf("second command = %q, want save-all flush", c)
	}
}

func TestRCONExecBadPassword(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		id, _, _, err := readRCON(bufio.NewReader(conn))
		if err != nil {
			return
		}
		_ = id
		_ = writeRCON(conn, rconAuthFailedID, rconTypeExec, "") // always reject
	}()

	if err := rconExec(ln.Addr().String(), "whatever", "save-off"); err == nil {
		t.Fatal("expected auth failure error")
	}
}
