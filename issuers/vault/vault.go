package vault

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hashicorp/vault/api"

	"github.com/utilitywarehouse/certify"
	"github.com/utilitywarehouse/certify/internal/csr"
)

// Issuer implements the Issuer interface with a
// Hashicorp Vault PKI Secrets Engine backend.
//
// URL, Token and Role are required.
type Issuer struct {
	// URL is the URL of the Vault instance.
	URL *url.URL
	// Token is the Vault secret token that should be used
	// when issuing certificates.
	Token string
	// Mount is the name under which the PKI secrets engine
	// is mounted. Defaults to `pki`
	Mount string
	// Role is the Vault Role that should be used
	// when issuing certificates.
	Role string
	// TLSConfig allows configuration of the TLS config
	// used when connecting to the Vault server.
	TLSConfig *tls.Config

	// TimeToLive configures the lifetime of certificates
	// requested from the Vault server.
	TimeToLive time.Duration
	// OtherSubjectAlternativeNames defines custom OID/UTF8-string SANs.
	// The format is the same as OpenSSL: <oid>;<type>:<value> where the only current valid <type> is UTF8.
	OtherSubjectAlternativeNames []string

	cli *api.Client
}

// FromClient returns an Issuer using the provided Vault API client.
// Any changes to the issuers properties (such as setting the TTL or adding Other SANS)
// must be done before using it. The client must have its token configured.
func FromClient(v *api.Client, role string) *Issuer {
	return &Issuer{
		Role: role,
		cli:  v,
	}
}

func (v *Issuer) connect(ctx context.Context) error {
	vConf := api.DefaultConfig()

	if v.TLSConfig != nil {
		vConf.HttpClient.Transport.(*http.Transport).TLSClientConfig = v.TLSConfig.Clone()
	}

	vConf.Address = v.URL.String()
	var err error
	v.cli, err = api.NewClient(vConf)
	if err != nil {
		return err
	}

	v.cli.SetToken(v.Token)
	return nil
}

// Issue issues a certificate from the configured Vault backend,
// establishing a connection if one doesn't already exist.
func (v *Issuer) Issue(ctx context.Context, commonName string, conf *certify.CertConfig) (*tls.Certificate, error) {
	if v.cli == nil { // Could be set by FromClient
		err := v.connect(ctx)
		if err != nil {
			return nil, err
		}
	}

	csrPEM, keyPEM, err := csr.FromCertConfig(commonName, conf)
	if err != nil {
		return nil, err
	}

	opts := csrOpts{
		CSR:               string(csrPEM),
		CommonName:        commonName,
		ExcludeCNFromSANS: true,
		Format:            "pem",
		OtherSans:         v.OtherSubjectAlternativeNames,
		TimeToLive:        ttl(v.TimeToLive),
	}

	secret, err := v.signCSR(ctx, opts)
	if err != nil {
		return nil, err
	}

	// https://www.vaultproject.io/api/secret/pki/index.html#sample-response-15
	certPEM := []byte(secret.Data["certificate"].(string))
	caChainPEM := certPEM
	if caChain, ok := secret.Data["ca_chain"]; ok {
		for _, pemData := range caChain.([]interface{}) {
			caChainPEM = append(append(caChainPEM, '\n'), []byte(pemData.(string))...)
		}
	} else if ca, ok := secret.Data["issuing_ca"]; ok {
		caChainPEM = append(append(caChainPEM, '\n'), []byte(ca.(string))...)
	}

	tlsCert, err := tls.X509KeyPair(caChainPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	// This can't error since it's called in tls.X509KeyPair above successfully
	tlsCert.Leaf, _ = x509.ParseCertificate(tlsCert.Certificate[0])
	return &tlsCert, nil
}

func (v Issuer) signCSR(ctx context.Context, opts csrOpts) (*api.Secret, error) {
	pkiMountName := "pki"
	if v.Mount != "" {
		pkiMountName = v.Mount
	}

	r := v.cli.NewRequest("PUT", "/v1/"+pkiMountName+"/sign/"+v.Role)
	if err := r.SetJSONBody(opts); err != nil {
		return nil, err
	}

	resp, err := v.cli.RawRequestWithContext(ctx, r)
	if resp != nil {
		defer resp.Body.Close()
	}
	if resp != nil && resp.StatusCode == 404 {
		secret, parseErr := api.ParseSecret(resp.Body)
		switch parseErr {
		case nil:
		case io.EOF:
			return nil, nil
		default:
			return nil, err
		}
		if secret != nil && (len(secret.Warnings) > 0 || len(secret.Data) > 0) {
			return secret, err
		}
	}
	if err != nil {
		return nil, err
	}

	return api.ParseSecret(resp.Body)
}
