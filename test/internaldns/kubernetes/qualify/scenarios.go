// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/miekg/dns"
	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *suite) scenarios(a, b *project) error {
	for _, p := range []*project{a, b} {
		if err := s.expect(p, "only-a.prod.internal", "A", false, dns.RcodeNameError); err != nil {
			return err
		}
	}
	pa, err := s.publish(a, objectRef(&a.zone), "common", "common", "10.42.0.10", "fd42::10")
	if err != nil {
		return err
	}
	pb, err := s.publish(b, objectRef(&b.zone), "common", "common", "10.42.0.20", "fd42::20")
	if err != nil {
		return err
	}
	if pa.registration.UID == pb.registration.UID || pa.grant.UID == pb.grant.UID || pa.contribution.UID == pb.contribution.UID {
		return fmt.Errorf("same-named project publications lost their distinct identities")
	}
	s.pass("same-named registrations, grants, and contributions retain independent source identities", nil)
	foreign := &dnsv1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "foreign-project-endpoint", Namespace: a.Namespace}, Spec: dnsv1.DNSRecordContributionSpec{RegistrationRef: objectRef(&pb.registration), GrantRef: objectRef(&pb.grant), RecordSets: recordSets("common", "10.42.0.99", "")}}
	if err := a.product.Create(s.ctx, foreign); !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return fmt.Errorf("another project's publication references must be rejected by admission: %v", err)
	}
	s.pass("product admission rejects publication references from another source project", nil)
	for _, tcp := range []bool{false, true} {
		if err := s.expect(a, "common.prod.internal", "A", tcp, dns.RcodeSuccess, "10.42.0.10"); err != nil {
			return err
		}
		if err := s.expect(b, "common.prod.internal", "A", tcp, dns.RcodeSuccess, "10.42.0.20"); err != nil {
			return err
		}
		if err := s.expect(a, "common.prod.internal", "AAAA", tcp, dns.RcodeSuccess, "fd42::10"); err != nil {
			return err
		}
		if err := s.expect(b, "common.prod.internal", "AAAA", tcp, dns.RcodeSuccess, "fd42::20"); err != nil {
			return err
		}
	}
	for _, p := range []*project{a, b} {
		if err := s.expect(p, "common.prod.internal", "TXT", false, dns.RcodeSuccess); err != nil {
			return err
		}
	}
	unknown := *a
	unknown.ConsumerAddress = "fd53::dead"
	for _, tcp := range []bool{false, true} {
		answer, err := s.query(&unknown, "common.prod.internal", "A", tcp)
		if err != nil {
			return err
		}
		if answer.Error == "" && answer.Rcode != dns.RcodeRefused && answer.Rcode != dns.RcodeServerFailure {
			return fmt.Errorf("unknown destination must not resolve a tenant's record: %+v", answer)
		}
	}
	if err := s.expect(b, "common.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	s.pass("an unbound destination cannot resolve either tenant's private records", nil)
	if _, err := s.publish(a, objectRef(&a.zone), "only-a", "only-a", "10.42.0.20", ""); err != nil {
		return err
	}
	if err := s.expect(a, "only-a.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	if err := s.expect(b, "only-a.prod.internal", "A", true, dns.RcodeNameError); err != nil {
		return err
	}
	s.pass("negative caches and overlapping endpoint address spaces stay isolated", nil)
	// Products discover their managed zone through context status.
	if _, err := s.publish(a, a.resolverContext.Status.ManagedNamespace.DNSZoneRef, "managed", "web", "10.42.0.11", ""); err != nil {
		return err
	}
	managedName := "web." + a.resolverContext.Status.ManagedNamespace.Suffix
	for _, tcp := range []bool{false, true} {
		if err := s.expect(b, managedName, "A", tcp, dns.RcodeNameError); err != nil {
			return err
		}
	}
	if _, err := s.publish(b, b.resolverContext.Status.ManagedNamespace.DNSZoneRef, "managed", "web", "10.42.0.21", ""); err != nil {
		return err
	}
	for _, tcp := range []bool{false, true} {
		if err := s.expect(a, managedName, "A", tcp, dns.RcodeSuccess, "10.42.0.11"); err != nil {
			return err
		}
		if err := s.expect(b, managedName, "A", tcp, dns.RcodeSuccess, "10.42.0.21"); err != nil {
			return err
		}
	}
	s.pass("the same hostname in the shared managed domain resolves within its own context over UDP and TCP", managedName)
	apps := &dnsv1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: a.Namespace}, Spec: dnsv1.DNSZoneSpec{DomainName: "apps.internal", DNSZoneClassName: "private-bind", Visibility: dnsv1.DNSZoneVisibilityPrivate}}
	if err := a.customer.Create(s.ctx, apps); err != nil {
		return err
	}
	if err := s.associate(a, apps, "apps"); err != nil {
		return err
	}
	if _, err := s.publish(a, objectRef(apps), "apps", "api", "10.42.0.12", ""); err != nil {
		return err
	}
	if err := s.expect(a, "api.apps.internal", "A", true, dns.RcodeSuccess, "10.42.0.12"); err != nil {
		return err
	}
	s.pass("one resolver context serves its managed namespace and multiple custom zones", nil)
	if err := s.observe(b, pb, true); err != nil {
		return err
	}
	if err := s.update(a, pa, "10.42.0.30", "fd42::30"); err != nil {
		return err
	}
	if err := s.expect(a, "common.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.30"); err != nil {
		return err
	}
	if err := s.expect(b, "common.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	if err := s.observe(a, pa, false); err != nil {
		return err
	}
	for _, tcp := range []bool{false, true} {
		if err := s.expect(a, "common.prod.internal", "A", tcp, dns.RcodeSuccess); err != nil {
			return err
		}
	}
	s.pass("an unhealthy endpoint stops producing DNS answers", nil)
	if err := s.observe(a, pa, true); err != nil {
		return err
	}
	if err := s.expect(a, "common.prod.internal", "A", true, dns.RcodeSuccess, "10.42.0.30"); err != nil {
		return err
	}
	if err := a.product.Get(s.ctx, client.ObjectKeyFromObject(&pa.contribution), &pa.contribution); err != nil {
		return err
	}
	stale := pa.contribution.DeepCopy()
	stale.Status.Sequence--
	if err := a.product.Status().Patch(s.ctx, stale, client.MergeFrom(&pa.contribution)); err == nil {
		return fmt.Errorf("stale producer sequence was accepted")
	} else if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return fmt.Errorf("stale producer sequence failed for an unrelated reason: %w", err)
	}
	s.pass("admission rejects a stale producer observation", nil)
	if err := s.takeover(a, pa); err != nil {
		return err
	}
	if err := s.observe(b, pb, true); err != nil {
		return err
	}
	deleted, err := s.publish(a, objectRef(&a.zone), "deleted", "deleted", "10.42.0.14", "")
	if err != nil {
		return err
	}
	if err := s.expect(a, "deleted.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.14"); err != nil {
		return err
	}
	if err := a.product.Delete(s.ctx, &deleted.contribution); err != nil {
		return err
	}
	if err := s.expect(a, "deleted.prod.internal", "A", true, dns.RcodeSuccess); err != nil {
		return err
	}
	s.pass("deleting a contribution withdraws its answer and preserves registration ownership", nil)
	if err := s.offlineServing(a, b); err != nil {
		return err
	}
	if err := s.observe(b, pb, true); err != nil {
		return err
	}
	if err := s.expect(b, "common.prod.internal", "A", true, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	s.pass("restored controllers and persistent broker publish a fresh observation", nil)
	return nil
}

func (s *suite) update(p *project, pub *publication, address, ipv6 string) error {
	if err := p.product.Get(s.ctx, client.ObjectKeyFromObject(&pub.contribution), &pub.contribution); err != nil {
		return err
	}
	before := pub.contribution.DeepCopy()
	pub.contribution.Spec.RecordSets = recordSets(pub.registration.Spec.Name, address, ipv6)
	if err := p.product.Patch(s.ctx, &pub.contribution, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.observe(p, pub, true)
}

func (s *suite) owner(zoneUID string) (dnsv1.DNSPublicationOwnership, error) {
	var values dnsv1.DNSPublicationOwnershipList
	if err := s.platform.List(s.ctx, &values, client.InNamespace(s.env.Namespace)); err != nil {
		return dnsv1.DNSPublicationOwnership{}, err
	}
	for _, value := range values.Items {
		if string(value.Spec.ZoneUID) == zoneUID {
			return value, nil
		}
	}
	return dnsv1.DNSPublicationOwnership{}, fmt.Errorf("zone %s has no publication owner", zoneUID)
}

func (s *suite) scaleController(name string, replicas int32) error {
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: s.env.Namespace, Name: name}
	if err := s.platform.Get(s.ctx, key, deployment); err != nil {
		return err
	}
	before := deployment.DeepCopy()
	deployment.Spec.Replicas = &replicas
	if err := s.platform.Patch(s.ctx, deployment, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.wait("controller scale "+name, 60*time.Second, func() (bool, error) {
		var pods corev1.PodList
		err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace), client.MatchingLabels(deployment.Spec.Selector.MatchLabels))
		if err != nil {
			return false, err
		}
		if replicas == 0 {
			return len(pods.Items) == 0, nil
		}
		if err := s.platform.Get(s.ctx, key, deployment); err != nil {
			return false, err
		}
		return deployment.Status.ObservedGeneration >= deployment.Generation &&
			deployment.Status.AvailableReplicas == replicas && readyPods(pods.Items, replicas), nil
	})
}

func (s *suite) takeover(p *project, pub *publication) (resultErr error) {
	initial, err := s.owner(string(p.zone.UID))
	if err != nil {
		return err
	}
	var target string
	for _, name := range s.env.ControllerDeployments {
		deployment := &appsv1.Deployment{}
		if err := s.platform.Get(s.ctx, client.ObjectKey{Namespace: s.env.Namespace, Name: name}, deployment); err != nil {
			return err
		}
		var pods corev1.PodList
		if err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace), client.MatchingLabels(deployment.Spec.Selector.MatchLabels)); err != nil {
			return err
		}
		for _, pod := range pods.Items {
			if strings.HasPrefix(initial.Spec.HolderIdentity, pod.Name+"-") {
				target = name
			}
		}
	}
	if target == "" {
		return fmt.Errorf("cannot identify current controller owner %s", initial.Spec.HolderIdentity)
	}
	defer func() {
		if err := s.scaleController(target, 1); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("controller restoration: %w", err))
		}
	}()
	if err := s.scaleController(target, 0); err != nil {
		return err
	}
	var current dnsv1.DNSPublicationOwnership
	if err := s.wait("ownership takeover by surviving controller", 90*time.Second, func() (bool, error) {
		var err error
		current, err = s.owner(string(p.zone.UID))
		return current.Spec.WriterEpoch > initial.Spec.WriterEpoch && current.Spec.HolderIdentity != initial.Spec.HolderIdentity, err
	}); err != nil {
		return err
	}
	if err := s.observe(p, pub, true); err != nil {
		return err
	}
	if err := s.expect(p, "common.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.30"); err != nil {
		return err
	}
	s.pass("surviving Kubernetes controller takes ownership at a higher writer epoch", map[string]any{"previous": initial.Spec.HolderIdentity, "current": current.Spec.HolderIdentity, "writerEpoch": current.Spec.WriterEpoch})
	return nil
}

func (s *suite) scaleBroker(replicas int32) error {
	set := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: s.env.Namespace, Name: s.env.BrokerStatefulSet}
	if err := s.platform.Get(s.ctx, key, set); err != nil {
		return err
	}
	before := set.DeepCopy()
	set.Spec.Replicas = &replicas
	if err := s.platform.Patch(s.ctx, set, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.wait("broker scale", 90*time.Second, func() (bool, error) {
		var pods corev1.PodList
		if err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace),
			client.MatchingLabels(set.Spec.Selector.MatchLabels)); err != nil {
			return false, err
		}
		if replicas == 0 {
			return len(pods.Items) == 0, nil
		}
		if err := s.platform.Get(s.ctx, key, set); err != nil {
			return false, err
		}
		return set.Status.ObservedGeneration >= set.Generation &&
			set.Status.ReadyReplicas == replicas && readyPods(pods.Items, replicas), nil
	})
}

// offlineServing checks uncached queries while control-plane delivery is unavailable.
func (s *suite) offlineServing(a, b *project) (resultErr error) {
	var broker appsv1.StatefulSet
	if err := s.platform.Get(s.ctx, client.ObjectKey{Namespace: s.env.Namespace, Name: s.env.BrokerStatefulSet}, &broker); err != nil {
		return err
	}
	replicas := int32(3)
	if broker.Spec.Replicas != nil {
		replicas = *broker.Spec.Replicas
	}
	defer func() {
		if err := s.scaleBroker(replicas); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("broker restoration: %w", err))
		}
		for _, name := range s.env.ControllerDeployments {
			if err := s.scaleController(name, 1); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("controller restoration: %w", err))
			}
		}
	}()
	for _, name := range s.env.ControllerDeployments {
		if err := s.scaleController(name, 0); err != nil {
			return err
		}
	}
	if err := s.scaleBroker(0); err != nil {
		return err
	}
	// The outage spans multiple DNS cache and fleet acknowledgement intervals.
	until := time.Now().Add(3 * time.Minute)
	for time.Now().Before(until) {
		for _, tcp := range []bool{false, true} {
			for _, tenant := range []struct {
				project *project
				address string
			}{{a, "10.42.0.30"}, {b, "10.42.0.20"}} {
				answer, err := s.query(tenant.project, "common.prod.internal", "A", tcp)
				if err != nil {
					return err
				}
				if answer.Error != "" || answer.Rcode != dns.RcodeSuccess || len(answer.Addresses) != 1 || answer.Addresses[0] != tenant.address {
					return fmt.Errorf("outage query for %s TCP=%t: %#v", tenant.project.Name, tcp, answer)
				}
			}
		}
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(time.Second):
		}
	}
	s.pass("both tenants keep resolving over UDP and TCP with controllers and messaging stopped", nil)
	var pods corev1.PodList
	if err := s.platform.List(s.ctx, &pods, client.InNamespace(s.env.Namespace), client.MatchingLabels{"internal-dns-role": "node"}); err != nil {
		return err
	}
	if len(pods.Items) != 1 {
		return fmt.Errorf("expected one node resolver, got %d", len(pods.Items))
	}
	pod := pods.Items[0]
	var restarts int32
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "agent" {
			restarts = status.RestartCount
		}
	}
	cmd := exec.CommandContext(s.ctx, "kubectl", "--kubeconfig", s.env.PlatformKubeconfig, "-n", s.env.Namespace, "exec", pod.Name, "-c", "agent", "--", "pkill", "-TERM", "-f", "^/internal-dns --role=agent")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart resolver agent: %w: %s", err, output)
	}
	if err := s.wait("resolver agent restart without messaging", time.Minute, func() (bool, error) {
		var current corev1.Pod
		if err := s.platform.Get(s.ctx, client.ObjectKeyFromObject(&pod), &current); err != nil {
			return false, err
		}
		for _, status := range current.Status.ContainerStatuses {
			if status.Name == "agent" && status.RestartCount > restarts && status.State.Running != nil {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	for _, tcp := range []bool{false, true} {
		if err := s.expect(a, "common.prod.internal", "A", tcp, dns.RcodeSuccess, "10.42.0.30"); err != nil {
			return err
		}
		if err := s.expect(b, "common.prod.internal", "A", tcp, dns.RcodeSuccess, "10.42.0.20"); err != nil {
			return err
		}
	}
	s.pass("resolver agent restores both tenant contexts from persistent state without messaging", nil)
	return nil
}

func readyPods(pods []corev1.Pod, replicas int32) bool {
	if len(pods) != int(replicas) {
		return false
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			return false
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return false
		}
	}
	return true
}
