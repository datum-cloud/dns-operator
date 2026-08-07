package dns

import (
	"fmt"

	"go.miloapis.com/dns-operator/internal/dns/fake"
	"go.miloapis.com/dns-operator/internal/dns/pdns"
)

type DNSHandler struct {
	Client *DNSClient
}

func New(className string, classType string) (*DNSHandler, error) {

	if classType == "powerdns" {
		client, err := pdns.NewFromEnv()
		if err != nil {
			return nil, err
		}
		return &DNSHandler{
			Client: &DNSClient{
				Name:          className,
				Type:          classType,
				DNSController: client,
			},
		}, nil
	}

	if classType == "fake" {
		client := fake.NewFakeDNSClient()
		return &DNSHandler{
			Client: &DNSClient{
				Name:          className,
				Type:          classType,
				DNSController: client,
			},
		}, nil
	}

	return nil, fmt.Errorf("unknown class %s and type %s", className, classType)
}
