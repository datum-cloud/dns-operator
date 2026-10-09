// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"fmt"
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
	pa, err := s.publish(a, objectRef(&a.zone), "common", "common", "10.42.0.10", "fd42::10", 90*time.Second)
	if err != nil {
		return err
	}
	pb, err := s.publish(b, objectRef(&b.zone), "common", "common", "10.42.0.20", "fd42::20", 90*time.Second)
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
	if _, err := s.publish(a, objectRef(&a.zone), "only-a", "only-a", "10.42.0.20", "", 90*time.Second); err != nil {
		return err
	}
	if err := s.expect(a, "only-a.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	if err := s.expect(b, "only-a.prod.internal", "A", true, dns.RcodeNameError); err != nil {
		return err
	}
	s.pass("negative caches and overlapping endpoint address spaces stay isolated", nil)
	// The product discovers the zone from context status rather than accepting a
	// zone selection on the Compute resource.
	if _, err := s.publish(a, a.resolverContext.Status.ManagedNamespace.DNSZoneRef, "managed", "web.instances", "10.42.0.11", "", 90*time.Second); err != nil {
		return err
	}
	managedName := "web.instances." + a.resolverContext.Status.ManagedNamespace.Suffix
	if err := s.expect(a, managedName, "A", false, dns.RcodeSuccess, "10.42.0.11"); err != nil {
		return err
	}
	apps := &dnsv1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "apps", Namespace: a.Namespace}, Spec: dnsv1.DNSZoneSpec{DomainName: "apps.internal", DNSZoneClassName: "private-bind", Visibility: dnsv1.DNSZoneVisibilityPrivate}}
	if err := a.admin.Create(s.ctx, apps); err != nil {
		return err
	}
	if err := s.associate(a, apps, "apps"); err != nil {
		return err
	}
	if _, err := s.publish(a, objectRef(apps), "apps", "api", "10.42.0.12", "", 90*time.Second); err != nil {
		return err
	}
	if err := s.expect(a, "api.apps.internal", "A", true, dns.RcodeSuccess, "10.42.0.12"); err != nil {
		return err
	}
	s.pass("one resolver context serves its managed namespace and multiple custom zones", nil)
	if err := s.observe(b, pb, true, 90*time.Second); err != nil {
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
	if err := s.observe(a, pa, false, 90*time.Second); err != nil {
		return err
	}
	for _, tcp := range []bool{false, true} {
		if err := s.expect(a, "common.prod.internal", "A", tcp, dns.RcodeSuccess); err != nil {
			return err
		}
	}
	s.pass("an unhealthy endpoint stops producing DNS answers", nil)
	if err := s.observe(a, pa, true, 90*time.Second); err != nil {
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
	if err := a.product.Status().Update(s.ctx, stale); err == nil {
		return fmt.Errorf("stale producer sequence was accepted")
	} else if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return fmt.Errorf("stale producer sequence failed for an unrelated reason: %w", err)
	}
	s.pass("admission rejects a stale producer observation", nil)
	if err := s.takeover(a, pa); err != nil {
		return err
	}
	if err := s.observe(b, pb, true, 90*time.Second); err != nil {
		return err
	}
	deleted, err := s.publish(a, objectRef(&a.zone), "deleted", "deleted", "10.42.0.14", "", 90*time.Second)
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
	if err := s.observe(a, pa, true, 90*time.Second); err != nil {
		return err
	}
	if err := s.observe(b, pb, true, 90*time.Second); err != nil {
		return err
	}
	if err := s.offlineExpiry(a, b, pa, pb); err != nil {
		return err
	}
	if err := s.observe(b, pb, true, 90*time.Second); err != nil {
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
	return s.observe(p, pub, true, 90*time.Second)
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
	if err := s.observe(p, pub, true, 90*time.Second); err != nil {
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

func (s *suite) offlineExpiry(a, b *project, pa, pb *publication) (resultErr error) {
	// Keep the healthy tenant's original record and access lease long enough to
	// cover the outage. Only the expired tenant receives the short access lease.
	if err := s.observe(a, pa, true, 90*time.Second); err != nil {
		return err
	}
	if err := s.observe(b, pb, true, 90*time.Second); err != nil {
		return err
	}
	if err := s.renewAccess(b, 5*time.Minute); err != nil {
		return err
	}
	expiring, err := s.publish(a, objectRef(&a.zone), "expires", "expires", "10.42.0.15", "", 30*time.Second)
	if err != nil {
		return err
	}
	if err := s.expect(a, "expires.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.15"); err != nil {
		return err
	}
	if err := a.integration.Get(s.ctx, client.ObjectKeyFromObject(&a.access), &a.access); err != nil {
		return err
	}
	before := a.access.DeepCopy()
	a.access.Spec.Authorization.Sequence++
	a.access.Spec.Authorization.ValidUntil = metav1.NewTime(time.Now().Add(50 * time.Second).UTC())
	if err := a.integration.Patch(s.ctx, &a.access, client.MergeFrom(before)); err != nil {
		return err
	}
	if err := s.waitAccess(a); err != nil {
		return err
	}
	var broker appsv1.StatefulSet
	if err := s.platform.Get(s.ctx, client.ObjectKey{Namespace: s.env.Namespace, Name: s.env.BrokerStatefulSet}, &broker); err != nil {
		return err
	}
	brokerReplicas := int32(1)
	if broker.Spec.Replicas != nil {
		brokerReplicas = *broker.Spec.Replicas
	}
	defer func() {
		if err := s.scaleBroker(brokerReplicas); err != nil {
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
	if expiring.contribution.Status.ValidUntil == nil || !expiring.contribution.Status.ValidUntil.After(time.Now().Add(3*time.Second)) {
		return fmt.Errorf("record expired before the outage; no offline expiry proof")
	}
	if !a.access.Spec.Authorization.ValidUntil.After(time.Now().Add(10 * time.Second)) {
		return fmt.Errorf("access expired before the outage; no offline authorization proof")
	}
	if err := s.expect(a, "expires.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.15"); err != nil {
		return err
	}
	s.pass("Kubernetes controllers and broker stop without stopping the serving pods", nil)
	if deadline := expiring.contribution.Status.ValidUntil; deadline != nil {
		if remaining := time.Until(deadline.Add(3 * time.Second)); remaining > 0 {
			time.Sleep(remaining)
		}
	}
	if err := s.expect(a, "expires.prod.internal", "A", false, dns.RcodeSuccess); err != nil {
		return err
	}
	if err := s.expect(b, "common.prod.internal", "A", true, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	s.pass("local record expiry preserves the original deadline during a broker and controller outage", expiring.contribution.Status.ValidUntil)
	if remaining := time.Until(a.access.Spec.Authorization.ValidUntil.Add(3 * time.Second)); remaining > 0 {
		time.Sleep(remaining)
	}
	for _, tcp := range []bool{false, true} {
		if err := s.wait("expired access rejected", 15*time.Second, func() (bool, error) {
			answer, err := s.query(a, "common.prod.internal", "A", tcp)
			if err != nil {
				return false, err
			}
			return answer.Error != "" || answer.Rcode == dns.RcodeRefused || answer.Rcode == dns.RcodeServerFailure, nil
		}); err != nil {
			return err
		}
	}
	if err := s.expect(b, "common.prod.internal", "A", false, dns.RcodeSuccess, "10.42.0.20"); err != nil {
		return err
	}
	s.pass("expired access fails closed while another tenant keeps resolving", a.access.Spec.Authorization.ValidUntil)
	return nil
}

func (s *suite) renewAccess(p *project, lifetime time.Duration) error {
	if err := p.integration.Get(s.ctx, client.ObjectKeyFromObject(&p.access), &p.access); err != nil {
		return err
	}
	before := p.access.DeepCopy()
	p.access.Spec.Authorization.Sequence++
	p.access.Spec.Authorization.ValidUntil = metav1.NewTime(time.Now().Add(lifetime).UTC())
	if err := p.integration.Patch(s.ctx, &p.access, client.MergeFrom(before)); err != nil {
		return err
	}
	return s.waitAccess(p)
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
