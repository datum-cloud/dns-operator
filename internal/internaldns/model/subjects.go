package model

import "fmt"

func ServingSubject(region, shard string) string {
	return fmt.Sprintf("dns.private.serving.%s.%s", SafeToken(region), SafeToken(shard))
}

func RecordChunkSubject(region, shard, zoneUID string) string {
	return fmt.Sprintf("dns.private.records.%s.%s.%s.chunk", SafeToken(region), SafeToken(shard), OpaqueToken(zoneUID))
}

func RecordManifestSubject(region, shard, zoneUID string) string {
	return fmt.Sprintf("dns.private.records.%s.%s.%s.manifest", SafeToken(region), SafeToken(shard), OpaqueToken(zoneUID))
}

func RecordConsumerFilter(region, shard string) string {
	return fmt.Sprintf("dns.private.records.%s.%s.>", SafeToken(region), SafeToken(shard))
}

func AckSubject(region, shard, memberID string) string {
	return fmt.Sprintf("dns.private.acks.%s.%s.%s", SafeToken(region), SafeToken(shard), OpaqueToken(memberID))
}
