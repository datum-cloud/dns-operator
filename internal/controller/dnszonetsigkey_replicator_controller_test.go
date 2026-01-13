package controller

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestTsigKeySecretLen_EqualsHashOutputSize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		alg  dnsv1alpha1.TSIGAlgorithm
		want int
	}{
		{"hmac-md5", dnsv1alpha1.TSIGAlgorithmHMACMD5, 16},
		{"hmac-sha1", dnsv1alpha1.TSIGAlgorithmHMACSHA1, 20},
		{"hmac-sha224", dnsv1alpha1.TSIGAlgorithmHMACSHA224, 28},
		{"hmac-sha256", dnsv1alpha1.TSIGAlgorithmHMACSHA256, 32},
		{"hmac-sha384", dnsv1alpha1.TSIGAlgorithmHMACSHA384, 48},
		{"hmac-sha512", dnsv1alpha1.TSIGAlgorithmHMACSHA512, 64},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tsigKeySecretLen(tc.alg); got != tc.want {
				t.Fatalf("tsigKeySecretLen(%q)=%d, want %d", tc.alg, got, tc.want)
			}
		})
	}
}
