// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package prog

import (
	"bytes"
	"testing"
)

func TestChecksumIP(t *testing.T) {
	tests := []struct {
		data string
		csum uint16
	}{
		{
			"",
			0xffff,
		},
		{
			"\x00",
			0xffff,
		},
		{
			"\x00\x00",
			0xffff,
		},
		{
			"\x00\x00\xff\xff",
			0x0000,
		},
		{
			"\xfc",
			0x03ff,
		},
		{
			"\xfc\x12",
			0x03ed,
		},
		{
			"\xfc\x12\x3e",
			0xc5ec,
		},
		{
			"\xfc\x12\x3e\x00\xc5\xec",
			0x0000,
		},
		{
			"\x42\x00\x00\x43\x44\x00\x00\x00\x45\x00\x00\x00\xba\xaa\xbb\xcc\xdd",
			0xe143,
		},
		{
			"\x00\x00\x42\x00\x00\x43\x44\x00\x00\x00\x45\x00\x00\x00\xba\xaa\xbb\xcc\xdd",
			0xe143,
		},
	}

	for _, test := range tests {
		csum := ipChecksum([]byte(test.data))
		if csum != test.csum {
			t.Fatalf("incorrect ip checksum, got: %x, want: %x, data: %+v", csum, test.csum, []byte(test.data))
		}
	}
}

func TestChecksumIPAcc(t *testing.T) {
	rs, iters := initTest(t)
	r := newRand(rs)

	for i := 0; i < iters; i++ {
		bytes := make([]byte, r.Intn(256))
		for i := 0; i < len(bytes); i++ {
			bytes[i] = byte(r.Intn(256))
		}
		step := int(r.randRange(1, 8)) * 2
		var csumAcc IPChecksum
		for i := 0; i < len(bytes)/step; i++ {
			csumAcc.Update(bytes[i*step : (i+1)*step])
		}
		if len(bytes)%step != 0 {
			csumAcc.Update(bytes[len(bytes)-(len(bytes)%step) : len(bytes)])
		}
		csum := ipChecksum(bytes)
		if csum != csumAcc.Digest() {
			t.Fatalf("inconsistent ip checksum: %x vs %x, step: %v, data: %+v", csum, csumAcc.Digest(), step, bytes)
		}
	}
}

func TestChecksumEncode(t *testing.T) {
	tests := []struct {
		prog    string
		encoded string
	}{
		{
			"syz_test$csum_encode(&(0x7f0000000000)={0x42, 0x43, [0x44, 0x45], 0xa, 0xb, \"aabbccdd\"})",
			"\x42\x00\x00\x43\x44\x00\x00\x00\x45\x00\x00\x00\xba\xaa\xbb\xcc\xdd",
		},
	}
	for i, test := range tests {
		p, err := Deserialize([]byte(test.prog))
		if err != nil {
			t.Fatalf("failed to deserialize prog %v: %v", test.prog, err)
		}
		encoded := encodeStruct(p.Calls[0].Args[0].Res, 0)
		if !bytes.Equal(encoded, []byte(test.encoded)) {
			t.Fatalf("incorrect encoding for prog #%v, got: %+v, want: %+v", i, encoded, []byte(test.encoded))
		}
	}
}

func TestChecksumIPv4Calc(t *testing.T) {
	tests := []struct {
		prog string
		csum uint16
	}{
		{
			"syz_test$csum_ipv4(&(0x7f0000000000)={0x0, {0x42, 0x43, [0x44, 0x45], 0xa, 0xb, \"aabbccdd\"}})",
			0xe143,
		},
	}
	for i, test := range tests {
		p, err := Deserialize([]byte(test.prog))
		if err != nil {
			t.Fatalf("failed to deserialize prog %v: %v", test.prog, err)
		}
		_, csumField := calcChecksumIPv4(p.Calls[0].Args[0].Res, i%32)
		// Can't compare serialized progs, since checksums are zerod on serialization.
		csum := csumField.Value(i % 32)
		if csum != uintptr(test.csum) {
			t.Fatalf("failed to calc ipv4 checksum, got %x, want %x, prog: '%v'", csum, test.csum, test.prog)
		}
	}
}

func TestChecksumCalcRandom(t *testing.T) {
	rs, iters := initTest(t)
	for i := 0; i < iters; i++ {
		p := Generate(rs, 10, nil)
		for _, call := range p.Calls {
			calcChecksumsCall(call, i%32)
		}
		for try := 0; try <= 10; try++ {
			p.Mutate(rs, 10, nil, nil)
			for _, call := range p.Calls {
				calcChecksumsCall(call, i%32)
			}
		}
	}
}
