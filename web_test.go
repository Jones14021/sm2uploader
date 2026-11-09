package main

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type captureHandler struct{ content []byte }

func (handler *captureHandler) Ping(*Printer) bool                { return true }
func (handler *captureHandler) Connect() error                    { return nil }
func (handler *captureHandler) Disconnect() error                 { return nil }
func (handler *captureHandler) SetToolTemperature(int, int) error { return nil }
func (handler *captureHandler) SetBedTemperature(int, int) error  { return nil }
func (handler *captureHandler) Home() error                       { return nil }
func (handler *captureHandler) Upload(payload *Payload) error {
	var err error
	handler.content, err = io.ReadAll(payload.File)
	return err
}

func TestWebUploadPreservesBytes(t *testing.T) {
	original := []byte("; header\r\nG1 X12.3\n\x00\xff\nM104 S210\n")
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "test.gcode")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	previous := Connector.handlers
	previousUpdate := OnPrinterUpdate
	defer func() { Connector.handlers = previous; OnPrinterUpdate = previousUpdate }()
	capture := &captureHandler{}
	Connector.handlers = []Handler{capture}
	OnPrinterUpdate = nil
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8889/api/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Sm2uploader", "1")
	response := httptest.NewRecorder()
	webHandler(&Printer{IP: "127.0.0.1"}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload returned %d: %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(capture.content, original) {
		t.Fatalf("upload modified bytes: %q", capture.content)
	}
}

func TestWebRejectsCrossOriginAndUntrustedHosts(t *testing.T) {
	handler := webHandler(&Printer{})
	for _, test := range []struct{ address, origin string }{
		{"http://evil.example:8889/", ""},
		{"http://127.0.0.1:8889/api/upload", "http://evil.example"},
	} {
		request := httptest.NewRequest(http.MethodPost, test.address, nil)
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d", test.address, response.Code)
		}
	}
}

func TestSACPReadFragmented(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	encoded := (SACP_pack{ReceiverID: 1, SenderID: 2, Sequence: 42, CommandSet: 1, CommandID: 32, Data: []byte{0, 1, 2}}).Encode()
	go func() {
		for _, value := range encoded {
			if _, err := server.Write([]byte{value}); err != nil {
				return
			}
		}
	}()
	packet, err := SACP_read(client, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Sequence != 42 || !bytes.Equal(packet.Data, []byte{0, 1, 2}) {
		t.Fatalf("unexpected packet: %+v", packet)
	}
}

func TestSACPParseHardware(t *testing.T) {
	modules, err := parseSACPModules([]byte{1, 3, 13, 0, 0, 0, 1, 0, 0, 0, 1, 3, 0, '1', '.', '0'})
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].ID != 13 || modules[0].Firmware != "1.0" {
		t.Fatalf("unexpected modules: %+v", modules)
	}
	data := make([]byte, 4+2*17)
	data[0], data[3] = 3, 2
	for index := 0; index < 2; index++ {
		entry := data[4+index*17:]
		entry[0] = byte(index)
		binary.LittleEndian.PutUint32(entry[5:9], uint32(400+index*200))
		binary.LittleEndian.PutUint32(entry[9:13], uint32(201500+index*1000))
		binary.LittleEndian.PutUint16(entry[13:15], 210)
	}
	nozzles, err := parseSACPNozzles(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(nozzles) != 2 || nozzles[0].Diameter != 0.4 || nozzles[1].Diameter != 0.6 || nozzles[1].Temperature != 202.5 || nozzles[1].Target != 210 {
		t.Fatalf("unexpected nozzles: %+v", nozzles)
	}
	if _, err := parseSACPModules([]byte{1, 1}); err == nil {
		t.Fatal("accepted truncated module")
	}
	if _, err := parseSACPNozzles(data[:8]); err == nil {
		t.Fatal("accepted truncated nozzle")
	}
	beds, err := parseSACPBeds([]byte{9, 1, 0, 0x54, 0xf2, 0, 0, 60, 0})
	if err != nil || len(beds) != 1 || beds[0].Temperature != 62.036 || beds[0].Target != 60 {
		t.Fatalf("unexpected bed: %+v, %v", beds, err)
	}
}

func TestSACPWorkflowSubscription(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		request, err := SACP_read(server, time.Second)
		if err != nil || request.CommandSet != 1 || request.CommandID != 0 || !bytes.Equal(request.Data, []byte{1, 0xa0, 0xe8, 3}) {
			return
		}
		server.Write((SACP_pack{Sequence: request.Sequence, CommandSet: 1, CommandID: 0, Data: []byte{0}}).Encode())
		server.Write((SACP_pack{Sequence: 6, CommandSet: 1, CommandID: 0xa0, Data: []byte{0, 2}}).Encode())
	}()
	state, err := sacpWorkflow(client)
	if err != nil || state != "Running" {
		t.Fatalf("workflow: %q, %v", state, err)
	}
}

func TestSACPStreamUploadPreservesBytes(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	content := bytes.Repeat([]byte("G1 X12.3\r\n\x00\xff"), 6000)
	result := make(chan error, 1)
	go func() {
		begin, err := SACP_read(server, 2*time.Second)
		if err != nil {
			result <- err
			return
		}
		if begin.CommandSet != 0xb0 || begin.CommandID != 0 {
			result <- fmt.Errorf("unexpected upload start")
			return
		}
		nameLen := int(binary.LittleEndian.Uint16(begin.Data[:2]))
		if binary.LittleEndian.Uint32(begin.Data[2+nameLen:]) != uint32(len(content)) {
			result <- fmt.Errorf("wrong upload size")
			return
		}
		checksum := md5.Sum(content)
		for _, chunk := range []uint16{1, 0} {
			request := bytes.Buffer{}
			writeSACPstring(&request, hex.EncodeToString(checksum[:]))
			writeLE(&request, chunk)
			if _, err = server.Write((SACP_pack{Sequence: chunk + 10, CommandSet: 0xb0, CommandID: 1, Data: request.Bytes()}).Encode()); err != nil {
				result <- err
				return
			}
			reply, err := SACP_read(server, 2*time.Second)
			if err != nil {
				result <- err
				return
			}
			data := reply.Data
			if len(data) < 39 {
				result <- fmt.Errorf("short chunk reply")
				return
			}
			hashLen := int(binary.LittleEndian.Uint16(data[1:3]))
			start := 3 + hashLen + 4
			if len(data) < start {
				result <- fmt.Errorf("short chunk header")
				return
			}
			offset := int(chunk) * SACP_data_len
			end := offset + SACP_data_len
			if end > len(content) {
				end = len(content)
			}
			if !bytes.Equal(data[start:], content[offset:end]) {
				result <- fmt.Errorf("chunk %d changed", chunk)
				return
			}
		}
		if _, err = server.Write((SACP_pack{CommandSet: 0xb0, CommandID: 2, Data: []byte{0}}).Encode()); err != nil {
			result <- err
			return
		}
		result <- nil
	}()
	if err := SACP_start_upload_reader(client, "test.gcode", bytes.NewReader(content), int64(len(content)), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
