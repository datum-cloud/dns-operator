// SPDX-License-Identifier: AGPL-3.0-only

package usage

import "google.golang.org/protobuf/encoding/protowire"

// PowerDNS PBDNSMessage field numbers from pdns/dnsmessage.proto.
const (
	pbFieldType     = 1
	pbFieldQuestion = 12
	pbFieldResponse = 13

	pbQuestionQName = 1
	pbQuestionQType = 2

	pbResponseRcode = 1

	pbTypeDNSResponse = 2
)

type pbMessage struct {
	typ   int
	qname string
	qtype uint32
	rcode uint32
}

func decodePBDNSMessage(b []byte) (pbMessage, bool) {
	var msg pbMessage
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return pbMessage{}, false
		}
		b = b[n:]
		switch {
		case num == pbFieldType && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return pbMessage{}, false
			}
			msg.typ = int(v)
			b = b[n:]
		case num == pbFieldQuestion && typ == protowire.BytesType:
			raw, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return pbMessage{}, false
			}
			b = b[n:]
			qname, qtype, ok := decodeQuestion(raw)
			if !ok {
				return pbMessage{}, false
			}
			msg.qname = qname
			msg.qtype = qtype
		case num == pbFieldResponse && typ == protowire.BytesType:
			raw, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return pbMessage{}, false
			}
			b = b[n:]
			rcode, ok := decodeResponse(raw)
			if !ok {
				return pbMessage{}, false
			}
			msg.rcode = rcode
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return pbMessage{}, false
			}
			b = b[n:]
		}
	}
	return msg, true
}

func decodeQuestion(b []byte) (string, uint32, bool) {
	var qname string
	var qtype uint32
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return "", 0, false
		}
		b = b[n:]
		switch {
		case num == pbQuestionQName && typ == protowire.BytesType:
			s, n := protowire.ConsumeString(b)
			if n < 0 {
				return "", 0, false
			}
			qname = s
			b = b[n:]
		case num == pbQuestionQType && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return "", 0, false
			}
			qtype = uint32(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return "", 0, false
			}
			b = b[n:]
		}
	}
	return qname, qtype, true
}

func decodeResponse(b []byte) (uint32, bool) {
	var rcode uint32
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0, false
		}
		b = b[n:]
		switch {
		case num == pbResponseRcode && typ == protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return 0, false
			}
			rcode = uint32(v)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return 0, false
			}
			b = b[n:]
		}
	}
	return rcode, true
}

func encodePBDNSMessage(typ int, qname string, qtype, rcode uint32) []byte {
	var b []byte
	b = protowire.AppendTag(b, pbFieldType, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(typ))

	var q []byte
	q = protowire.AppendTag(q, pbQuestionQName, protowire.BytesType)
	q = protowire.AppendString(q, qname)
	q = protowire.AppendTag(q, pbQuestionQType, protowire.VarintType)
	q = protowire.AppendVarint(q, uint64(qtype))
	b = protowire.AppendTag(b, pbFieldQuestion, protowire.BytesType)
	b = protowire.AppendBytes(b, q)

	var r []byte
	r = protowire.AppendTag(r, pbResponseRcode, protowire.VarintType)
	r = protowire.AppendVarint(r, uint64(rcode))
	b = protowire.AppendTag(b, pbFieldResponse, protowire.BytesType)
	b = protowire.AppendBytes(b, r)
	return b
}
