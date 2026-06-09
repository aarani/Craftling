package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Minimal Source RCON client (P5c), enough for the snapshot control server to
// flush a Minecraft server before freezing its disk. The protocol is a stream
// of little-endian packets: int32 length, then a length-counted body of int32
// request id, int32 type, a null-terminated ASCII command, and a trailing null
// pad. We only need AUTH and EXECCOMMAND.
//
// This lives in an untagged file (pure net/encoding) so its codec is unit
// tested on any host; the guest-only caller is in vsock_linux.go.

const (
	rconTypeAuth     int32 = 3 // SERVERDATA_AUTH
	rconTypeExec     int32 = 2 // SERVERDATA_EXECCOMMAND (also AUTH_RESPONSE)
	rconTypeRespVal  int32 = 0 // SERVERDATA_RESPONSE_VALUE
	rconAuthFailedID int32 = -1
	rconAuthID       int32 = 1
	rconExecID       int32 = 2
	rconMaxBody            = 4096 // generous cap on a single packet body
)

// rconExec connects to a Source RCON endpoint, authenticates, and runs each
// command in order. It is used to "save-off"/"save-all flush" before a snapshot
// and "save-on" after.
func rconExec(addr, password string, cmds ...string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("rcon dial %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	if err := writeRCON(conn, rconAuthID, rconTypeAuth, password); err != nil {
		return fmt.Errorf("rcon auth send: %w", err)
	}
	id, _, _, err := readRCON(br)
	if err != nil {
		return fmt.Errorf("rcon auth reply: %w", err)
	}
	if id == rconAuthFailedID {
		return errors.New("rcon auth failed (bad password)")
	}

	for _, c := range cmds {
		if err := writeRCON(conn, rconExecID, rconTypeExec, c); err != nil {
			return fmt.Errorf("rcon exec %q send: %w", c, err)
		}
		if _, _, _, err := readRCON(br); err != nil {
			return fmt.Errorf("rcon exec %q reply: %w", c, err)
		}
	}
	return nil
}

// writeRCON encodes one packet. Length counts everything after the length field
// itself: id(4) + type(4) + body + two null bytes.
func writeRCON(w io.Writer, id, typ int32, body string) error {
	payloadLen := 4 + 4 + len(body) + 2
	buf := make([]byte, 4+payloadLen)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(payloadLen))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(id))
	binary.LittleEndian.PutUint32(buf[8:12], uint32(typ))
	copy(buf[12:], body)
	// last two bytes already zero (body null terminator + packet pad)
	_, err := w.Write(buf)
	return err
}

// readRCON decodes one packet, returning its id, type, and body (without the
// trailing nulls). It rejects absurd lengths so a hostile or desynced peer
// can't make us allocate unboundedly.
func readRCON(r io.Reader) (id, typ int32, body string, err error) {
	var lenField [4]byte
	if _, err = io.ReadFull(r, lenField[:]); err != nil {
		return 0, 0, "", err
	}
	n := int(binary.LittleEndian.Uint32(lenField[:]))
	if n < 10 || n > rconMaxBody+10 {
		return 0, 0, "", fmt.Errorf("rcon: implausible packet length %d", n)
	}
	payload := make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, 0, "", err
	}
	id = int32(binary.LittleEndian.Uint32(payload[0:4]))
	typ = int32(binary.LittleEndian.Uint32(payload[4:8]))
	// body is payload[8:] minus the two trailing null bytes.
	b := payload[8:]
	b = b[:len(b)-2]
	return id, typ, string(b), nil
}
