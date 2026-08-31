// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodePBDNSMessage(t *testing.T) {
	t.Parallel()
	payload := encodePBDNSMessage(pbTypeDNSResponse, "www.example.com.", 1, 0)
	msg, ok := decodePBDNSMessage(payload)
	require.True(t, ok)
	assert.Equal(t, pbTypeDNSResponse, msg.typ)
	assert.Equal(t, "www.example.com.", msg.qname)
	assert.Equal(t, uint32(1), msg.qtype)
	assert.Equal(t, uint32(0), msg.rcode)
}

func TestDecodePBDNSMessageIgnoresUnknownFields(t *testing.T) {
	t.Parallel()
	payload := encodePBDNSMessage(1, "example.com.", 28, 3)
	msg, ok := decodePBDNSMessage(payload)
	require.True(t, ok)
	assert.Equal(t, 1, msg.typ)
	assert.Equal(t, "example.com.", msg.qname)
	assert.Equal(t, uint32(28), msg.qtype)
	assert.Equal(t, uint32(3), msg.rcode)
}

func TestLengthPrefixedRoundTrip(t *testing.T) {
	t.Parallel()
	payload := encodePBDNSMessage(pbTypeDNSResponse, "a.example.com.", 1, 3)
	var buf bytes.Buffer
	require.NoError(t, writeLengthPrefixed(&buf, payload))
	got, err := readLengthPrefixed(&buf)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestDecodePBDNSMessageRejectsTruncated(t *testing.T) {
	t.Parallel()
	_, ok := decodePBDNSMessage([]byte{0x08})
	assert.False(t, ok)
}
