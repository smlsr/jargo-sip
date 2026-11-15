package sip

import (
	"bytes"
	"net"
	"testing"
)

// RFC 5769 section 2.2. Mapped address 192.0.2.1 port 32853.
const rfc5769IPv4 = "\x01\x01\x00\x3c" +
	"\x21\x12\xa4\x42" +
	"\xb7\xe7\xa7\x01\xbc\x34\xd6\x86\xfa\x87\xdf\xae" +
	"\x80\x22\x00\x0b" +
	"\x74\x65\x73\x74\x20\x76\x65\x63\x74\x6f\x72\x20" +
	"\x00\x20\x00\x08" +
	"\x00\x01\xa1\x47\xe1\x12\xa6\x43" +
	"\x00\x08\x00\x14" +
	"\x2b\x91\xf5\x99\xfd\x9e\x90\xc3\x8c\x74\x89\xf9" +
	"\x2a\xf9\xba\x53\xf0\x6b\xe7\xd7" +
	"\x80\x28\x00\x04" +
	"\xc0\x7d\x4c\x96"

// RFC 5769 section 2.3. Mapped address 2001:db8:1234:5678:11:2233:4455:6677 port 32853.
const rfc5769IPv6 = "\x01\x01\x00\x48" +
	"\x21\x12\xa4\x42" +
	"\xb7\xe7\xa7\x01\xbc\x34\xd6\x86\xfa\x87\xdf\xae" +
	"\x80\x22\x00\x0b" +
	"\x74\x65\x73\x74\x20\x76\x65\x63\x74\x6f\x72\x20" +
	"\x00\x20\x00\x14" +
	"\x00\x02\xa1\x47" +
	"\x01\x13\xa9\xfa\xa5\xd3\xf1\x79" +
	"\xbc\x25\xf4\xb5\xbe\xd2\xb9\xd9" +
	"\x00\x08\x00\x14" +
	"\xa3\x82\x95\x4e\x4b\xe6\x7b\xf1\x17\x84\xc9\x7c" +
	"\x82\x92\xc2\x75\xbf\xe3\xed\x41" +
	"\x80\x28\x00\x04" +
	"\xc8\xfb\x0b\x4c"

func rfc5769Tx() [12]byte {
	var tx [12]byte
	copy(tx[:], []byte{0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae})
	return tx
}

func TestXORMappedAddressRFC5769(t *testing.T) {
	if len(rfc5769IPv4) != 80 {
		t.Fatalf("ipv4 vector length %d, want 80", len(rfc5769IPv4))
	}
	tx, ip, port, kind, err := inspectSTUN([]byte(rfc5769IPv4))
	if err != nil || kind != stunOK {
		t.Fatalf("ipv4 inspect: kind %d err %v", kind, err)
	}
	if tx != rfc5769Tx() {
		t.Fatalf("ipv4 tx %x", tx)
	}
	if port != 32853 || !ip.Equal(net.ParseIP("192.0.2.1")) {
		t.Fatalf("ipv4 mapped %v:%d", ip, port)
	}

	if len(rfc5769IPv6) != 92 {
		t.Fatalf("ipv6 vector length %d, want 92", len(rfc5769IPv6))
	}
	tx, ip, port, kind, err = inspectSTUN([]byte(rfc5769IPv6))
	if err != nil || kind != stunOK {
		t.Fatalf("ipv6 inspect: kind %d err %v", kind, err)
	}
	if tx != rfc5769Tx() {
		t.Fatalf("ipv6 tx %x", tx)
	}
	want6 := net.ParseIP("2001:db8:1234:5678:11:2233:4455:6677")
	if port != 32853 || !ip.Equal(want6) {
		t.Fatalf("ipv6 mapped %v:%d", ip, port)
	}
}

func TestEncodeXORMappedRFC5769(t *testing.T) {
	tx := rfc5769Tx()
	msg, err := encodeSuccess(tx, net.ParseIP("192.0.2.1"), 32853)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0x01, 0xa1, 0x47, 0xe1, 0x12, 0xa6, 0x43}
	if !bytes.Equal(msg[24:32], want) {
		t.Fatalf("ipv4 xor bytes %x", msg[24:32])
	}
	gotTx, ip, port, kind, err := inspectSTUN(msg)
	if err != nil || kind != stunOK || gotTx != tx || port != 32853 || !ip.Equal(net.ParseIP("192.0.2.1")) {
		t.Fatalf("ipv4 round trip %v:%d kind %d tx %x err %v", ip, port, kind, gotTx, err)
	}

	msg, err = encodeSuccess(tx, net.ParseIP("2001:db8:1234:5678:11:2233:4455:6677"), 32853)
	if err != nil {
		t.Fatal(err)
	}
	want6 := []byte{
		0x00, 0x02, 0xa1, 0x47,
		0x01, 0x13, 0xa9, 0xfa, 0xa5, 0xd3, 0xf1, 0x79,
		0xbc, 0x25, 0xf4, 0xb5, 0xbe, 0xd2, 0xb9, 0xd9,
	}
	if !bytes.Equal(msg[24:44], want6) {
		t.Fatalf("ipv6 xor bytes %x", msg[24:44])
	}
}
