// SPDX-License-Identifier: AGPL-3.0-only

package usage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"go.miloapis.com/billing/emission"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	shutdownFlushTimeout = 5 * time.Second
	acceptBackoff        = time.Second
)

var (
	_ manager.Runnable               = &Collector{}
	_ manager.LeaderElectionRunnable = &Collector{}
)

// Collector accepts PowerDNS protobuf query logs, attributes responses to
// hosted zones, and flushes delta usage events.
//
// Query volume is counted on every agent that answers queries. Writer
// pods attribute from DNSZone shadows; edge pods attribute from the
// compact DATUM-USAGE metadata LightningStream replicates with the LMDB.
type Collector struct {
	Client     client.Client
	Store      IdentityStore
	Recorder   emission.Recorder
	Location   string
	ListenAddr string
	Interval   time.Duration

	index    *ZoneIndex
	counters *counterMap
}

// NeedLeaderElection reports that query counting must run on every replica
// that answers queries, not only the elected writer.
func (c *Collector) NeedLeaderElection() bool { return false }

// Start listens for protobuf connections and periodically flushes counters.
func (c *Collector) Start(ctx context.Context) error {
	c.init()
	ln, err := net.Listen("tcp", c.ListenAddr)
	if err != nil {
		return fmt.Errorf("listening for powerdns protobuf: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-ctx.Done()
		return ln.Close()
	})
	g.Go(func() error { return c.acceptLoop(ctx, ln) })
	g.Go(func() error { return c.flushLoop(ctx) })
	err = g.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (c *Collector) init() {
	if c.index == nil {
		c.index = &ZoneIndex{}
	}
	if c.counters == nil {
		c.counters = newCounterMap()
	}
	if c.Interval <= 0 {
		c.Interval = 60 * time.Second
	}
	if c.Recorder == nil {
		c.Recorder = emission.NoopRecorder{}
	}
	if c.ListenAddr == "" {
		c.ListenAddr = "127.0.0.1:4242"
	}
}

func (c *Collector) acceptLoop(ctx context.Context, ln net.Listener) error {
	logger := log.FromContext(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A transient accept failure must not take down the manager
			// (and with it zone/record programming).
			logger.Error(err, "accepting powerdns protobuf connection")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(acceptBackoff):
			}
			continue
		}
		go c.handleConn(ctx, conn)
	}
}

func (c *Collector) handleConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	logger := log.FromContext(ctx)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(c.Interval + time.Minute)); err != nil {
			return
		}
		payload, err := readLengthPrefixed(conn)
		if err != nil {
			if ctx.Err() != nil || err == io.EOF {
				return
			}
			logger.V(1).Info("protobuf connection closed", "error", err)
			return
		}
		c.observe(payload)
	}
}

func (c *Collector) observe(payload []byte) {
	msg, ok := decodePBDNSMessage(payload)
	if !ok || msg.typ != pbTypeDNSResponse || msg.qname == "" {
		return
	}
	zone, ok := c.index.Lookup(msg.qname)
	if !ok {
		return
	}
	rt := recordTypeName(msg.qtype)
	if rt == "" {
		return
	}
	c.counters.add(queryKey{
		domain:     zone.Domain,
		rcode:      rcodeName(msg.rcode),
		recordType: rt,
	}, 1)
}

func (c *Collector) flushLoop(ctx context.Context) error {
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	c.refreshAndFlush(ctx)
	for {
		select {
		case <-ctx.Done():
			// The parent context is already cancelled; Record would fail
			// immediately if we reused it.
			flushCtx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
			c.flush(flushCtx)
			cancel()
			return nil
		case <-ticker.C:
			c.refreshAndFlush(ctx)
		}
	}
}

func (c *Collector) refreshAndFlush(ctx context.Context) {
	if err := c.refreshIndex(ctx); err != nil {
		log.FromContext(ctx).Error(err, "refreshing dnszone index for usage")
	}
	c.flush(ctx)
}

func (c *Collector) refreshIndex(ctx context.Context) error {
	zones, kubeErr := c.listKubeIdentities(ctx)
	if c.Store != nil {
		stored, err := c.Store.ListUsageIdentities(ctx)
		if err != nil {
			if len(zones) == 0 {
				if kubeErr != nil {
					return kubeErr
				}
				return fmt.Errorf("listing pdns usage identities: %w", err)
			}
			log.FromContext(ctx).Error(err, "listing pdns usage identities; using kube index")
		} else {
			zones = mergeIdentities(zones, stored)
		}
	} else if kubeErr != nil {
		return kubeErr
	}
	c.index.Replace(zones)
	return nil
}

func (c *Collector) listKubeIdentities(ctx context.Context) ([]ZoneIdentity, error) {
	if c.Client == nil {
		return nil, nil
	}
	var list dnsv1alpha1.DNSZoneList
	if err := c.Client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing dnszones for usage index: %w", err)
	}
	zones := make([]ZoneIdentity, 0, len(list.Items))
	for i := range list.Items {
		id, ok := IdentityFromZone(&list.Items[i])
		if ok {
			zones = append(zones, id)
		}
	}
	return zones, nil
}

// mergeIdentities unions two identity lists. First-seen domain wins so
// Kubernetes shadows take precedence over replicated PowerDNS metadata.
func mergeIdentities(primary, extra []ZoneIdentity) []ZoneIdentity {
	if len(extra) == 0 {
		return primary
	}
	seen := make(map[string]struct{}, len(primary)+len(extra))
	out := make([]ZoneIdentity, 0, len(primary)+len(extra))
	for _, id := range primary {
		d := NormalizeDomain(id.Domain)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, id)
	}
	for _, id := range extra {
		d := NormalizeDomain(id.Domain)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (c *Collector) flush(ctx context.Context) {
	snapshot := c.counters.snapshotAndReset()
	if len(snapshot) == 0 {
		return
	}
	now := time.Now()
	failed := make(map[queryKey]int64)
	logger := log.FromContext(ctx)
	for key, n := range snapshot {
		if n <= 0 {
			continue
		}
		zone, ok := c.index.get(key.domain)
		if !ok {
			continue
		}
		ev := eventForZone(MeterZoneQueries, zone, n, c.Location, map[string]string{
			DimRcode:      key.rcode,
			DimRecordType: key.recordType,
		}, now)
		if err := c.Recorder.Record(ctx, ev); err != nil {
			var ve *emission.ValidationError
			if errors.As(err, &ve) {
				logger.Error(err, "dropping invalid zone query usage event", "domain", key.domain)
				continue
			}
			logger.Error(err, "recording zone query usage", "domain", key.domain)
			failed[key] = n
		}
	}
	c.counters.restore(failed)
}

func readLengthPrefixed(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func writeLengthPrefixed(w io.Writer, payload []byte) error {
	if len(payload) > 0xffff {
		return fmt.Errorf("protobuf payload too large: %d", len(payload))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}
