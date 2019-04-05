package ca

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"time"

	"github.com/andres-erbsen/clock"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/spire/pkg/common/cryptoutil"
	"github.com/spiffe/spire/pkg/common/telemetry"
	"github.com/spiffe/spire/pkg/common/util"
	"github.com/spiffe/spire/pkg/server/catalog"
	"github.com/spiffe/spire/proto/spire/common"
	"github.com/spiffe/spire/proto/spire/server/datastore"
	"github.com/spiffe/spire/proto/spire/server/keymanager"
	"github.com/spiffe/spire/proto/spire/server/upstreamca"
	"github.com/zeebo/errs"
)

const (
	DefaultCATTL    = 24 * time.Hour
	backdate        = time.Second * 10
	rotateInterval  = time.Minute
	pruneInterval   = 6 * time.Hour
	safetyThreshold = 24 * time.Hour
)

type CASetter interface {
	SetX509CA(*X509CA)
	SetJWTKey(*JWTKey)
}

type ManagerConfig struct {
	CA             CASetter
	Catalog        catalog.Catalog
	TrustDomain    url.URL
	UpstreamBundle bool
	CATTL          time.Duration
	CASubject      pkix.Name
	Dir            string
	Log            logrus.FieldLogger
	Metrics        telemetry.Metrics
	Clock          clock.Clock
}

type Manager struct {
	c  ManagerConfig
	ca ServerCA

	currentX509CA *x509CASlot
	nextX509CA    *x509CASlot
	currentJWTKey *jwtKeySlot
	nextJWTKey    *jwtKeySlot

	journal *Journal
}

func NewManager(c ManagerConfig) *Manager {
	if c.CATTL <= 0 {
		c.CATTL = DefaultCATTL
	}
	if c.Clock == nil {
		c.Clock = clock.New()
	}

	return &Manager{
		c: c,
	}
}

func (m *Manager) Initialize(ctx context.Context) error {
	if err := m.loadJournal(ctx); err != nil {
		return err
	}
	if err := m.rotate(ctx); err != nil {
		return err
	}

	return nil
}

func (m *Manager) Run(ctx context.Context) error {
	err := util.RunTasks(ctx,
		func(ctx context.Context) error {
			return m.rotateEvery(ctx, rotateInterval)
		},
		func(ctx context.Context) error {
			return m.pruneBundleEvery(ctx, pruneInterval)
		})
	if err == context.Canceled {
		err = nil
	}
	return err
}

func (m *Manager) rotateEvery(ctx context.Context, interval time.Duration) error {
	ticker := m.c.Clock.Ticker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.rotate(ctx)
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Manager) rotate(ctx context.Context) error {
	x509CAErr := m.rotateX509CA(ctx)
	if x509CAErr != nil {
		m.c.Log.Error("unable to rotate X509 CA: %v", x509CAErr)
	}

	jwtKeyErr := m.rotateJWTKey(ctx)
	if jwtKeyErr != nil {
		m.c.Log.Error("unable to rotate JWT key: %v", jwtKeyErr)
	}

	return errs.Combine(x509CAErr, jwtKeyErr)
}

func (m *Manager) rotateX509CA(ctx context.Context) error {
	now := m.c.Clock.Now()

	// if there is no current keypair set, generate one
	if m.currentX509CA.IsEmpty() {
		if err := m.prepareX509CA(ctx, m.currentX509CA); err != nil {
			return err
		}
		m.activateX509CA()
	}

	// if there is no next keypair set and the current is within the
	// preparation threshold, generate one.
	if m.nextX509CA.IsEmpty() && m.currentX509CA.ShouldPrepareNext(now) {
		if err := m.prepareX509CA(ctx, m.nextX509CA); err != nil {
			return err
		}
	}

	if m.currentX509CA.ShouldActivateNext(now) {
		m.currentX509CA, m.nextX509CA = m.nextX509CA, m.currentX509CA
		m.nextX509CA.Reset()
		m.activateX509CA()
	}

	return nil
}

func (m *Manager) prepareX509CA(ctx context.Context, slot *x509CASlot) (err error) {
	defer telemetry.CountCall(m.c.Metrics, "manager", "x509_ca", "prepare")(&err)

	log := m.c.Log.WithField("slot", slot.id)
	log.Debug("Preparing X509 CA")

	slot.Reset()

	now := m.c.Clock.Now()
	notBefore := now.Add(-backdate)
	notAfter := now.Add(m.c.CATTL)

	km := m.c.Catalog.GetKeyManager()
	signer, err := cryptoutil.GenerateKeyAndSigner(ctx, km, slot.KmKeyID(), keymanager.KeyType_EC_P384)
	if err != nil {
		return err
	}

	upstreamCA, _ := m.c.Catalog.GetUpstreamCA()
	x509CA, trustBundle, err := SignX509CA(ctx, signer, upstreamCA, m.c.UpstreamBundle, m.c.TrustDomain.Host, m.c.CASubject, notBefore, notAfter)
	if err != nil {
		return err
	}

	if err := m.appendBundle(ctx, trustBundle, nil); err != nil {
		return err
	}

	slot.issuedAt = now
	slot.x509CA = x509CA

	if err := m.journal.AppendX509CA(slot.id, slot.issuedAt, slot.x509CA); err != nil {
		log.WithField("err", err.Error()).Error("Unable to append X509 CA to journal")
	}

	m.c.Log.WithFields(logrus.Fields{
		"slot":            slot.id,
		"issued_at":       timeField(slot.issuedAt),
		"not_after":       timeField(slot.x509CA.Chain[0].NotAfter),
		"self_signed":     upstreamCA == nil,
		"is_intermediate": slot.x509CA.IsIntermediate,
	}).Info("X509 CA prepared")
	return nil
}

func (m *Manager) activateX509CA() {
	m.c.Log.WithFields(logrus.Fields{
		"slot":      m.currentX509CA.id,
		"issued_at": timeField(m.currentX509CA.issuedAt),
		"not_after": timeField(m.currentX509CA.x509CA.Chain[0].NotAfter),
	}).Info("X509 CA activated")
	m.c.Metrics.IncrCounter([]string{"manager", "x509_ca", "activate"}, 1)
	m.c.CA.SetX509CA(m.currentX509CA.x509CA)
}

func (m *Manager) rotateJWTKey(ctx context.Context) error {
	now := m.c.Clock.Now()

	// if there is no current keypair set, generate one
	if m.currentJWTKey.IsEmpty() {
		if err := m.prepareJWTKey(ctx, m.currentJWTKey); err != nil {
			return err
		}
		m.activateJWTKey()
	}

	// if there is no next keypair set and the current is within the
	// preparation threshold, generate one.
	if m.nextJWTKey.IsEmpty() && m.currentJWTKey.ShouldPrepareNext(now) {
		if err := m.prepareJWTKey(ctx, m.nextJWTKey); err != nil {
			return err
		}
	}

	if m.currentJWTKey.ShouldActivateNext(now) {
		m.currentJWTKey, m.nextJWTKey = m.nextJWTKey, m.currentJWTKey
		m.nextJWTKey.Reset()
		m.activateJWTKey()
	}

	return nil
}

func (m *Manager) prepareJWTKey(ctx context.Context, slot *jwtKeySlot) (err error) {
	defer telemetry.CountCall(m.c.Metrics, "manager", "jwt_key", "prepare")(&err)

	log := m.c.Log.WithField("slot", slot.id)
	log.Debug("Preparing JWT key")

	slot.Reset()

	now := m.c.Clock.Now()
	notAfter := now.Add(m.c.CATTL)

	km := m.c.Catalog.GetKeyManager()
	signer, err := cryptoutil.GenerateKeyAndSigner(ctx, km, slot.KmKeyID(), keymanager.KeyType_EC_P256)
	if err != nil {
		return err
	}

	jwtKey, err := newJWTKey(signer, notAfter)
	if err != nil {
		return err
	}

	publicKey, err := publicKeyFromJWTKey(jwtKey)
	if err != nil {
		return err
	}

	if err := m.appendBundle(ctx, nil, publicKey); err != nil {
		return err
	}

	slot.issuedAt = now
	slot.jwtKey = jwtKey

	if err := m.journal.AppendJWTKey(slot.id, slot.issuedAt, slot.jwtKey); err != nil {
		log.WithField("err", err.Error()).Error("Unable to append JWT key to journal")
	}

	m.c.Log.WithFields(logrus.Fields{
		"slot":      slot.id,
		"issued_at": timeField(slot.issuedAt),
		"not_after": timeField(slot.jwtKey.NotAfter),
	}).Info("JWT key prepared")
	return nil
}

func (m *Manager) pruneBundleEvery(ctx context.Context, interval time.Duration) error {
	ticker := m.c.Clock.Ticker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := m.pruneBundle(ctx); err != nil {
				m.c.Log.Errorf("Could not prune CA certificates: %v", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Manager) activateJWTKey() {
	m.c.Log.WithFields(logrus.Fields{
		"slot":      m.currentJWTKey.id,
		"issued_at": timeField(m.currentJWTKey.issuedAt),
		"not_after": timeField(m.currentJWTKey.jwtKey.NotAfter),
	}).Info("JWT key activated")
	m.c.Metrics.IncrCounter([]string{"manager", "jwt_key", "activate"}, 1)
	m.c.CA.SetJWTKey(m.currentJWTKey.jwtKey)
}

func (m *Manager) pruneBundle(ctx context.Context) (err error) {
	defer telemetry.CountCall(m.c.Metrics, "manager", "bundle", "prune")(&err)
	ds := m.c.Catalog.GetDataStore()

	now := m.c.Clock.Now().Add(-safetyThreshold)

	resp, err := ds.FetchBundle(ctx, &datastore.FetchBundleRequest{
		TrustDomainId: m.c.TrustDomain.String(),
	})
	if err != nil {
		return errs.Wrap(err)
	}
	oldBundle := resp.Bundle
	if oldBundle == nil {
		// no bundle to prune
		return nil
	}

	newBundle := &common.Bundle{
		TrustDomainId: oldBundle.TrustDomainId,
	}
	changed := false
pruneRootCA:
	for _, rootCA := range oldBundle.RootCas {
		certs, err := x509.ParseCertificates(rootCA.DerBytes)
		if err != nil {
			return errs.Wrap(err)
		}
		// if any cert in the chain has expired beyond the safety
		// threshhold, throw the whole chain out
		for _, cert := range certs {
			if !cert.NotAfter.After(now) {
				m.c.Log.Infof("Pruning CA certificate number %v with expiry date %v", cert.SerialNumber, cert.NotAfter)
				changed = true
				continue pruneRootCA
			}
		}
		newBundle.RootCas = append(newBundle.RootCas, rootCA)
	}

	for _, jwtSigningKey := range oldBundle.JwtSigningKeys {
		notAfter := time.Unix(jwtSigningKey.NotAfter, 0)
		if !notAfter.After(now) {
			m.c.Log.Infof("Pruning JWT signing key %q with expiry date %v", jwtSigningKey.Kid, notAfter)
			changed = true
			continue
		}
		newBundle.JwtSigningKeys = append(newBundle.JwtSigningKeys, jwtSigningKey)
	}

	if len(newBundle.RootCas) == 0 {
		m.c.Log.Warn("Pruning halted; all known CA certificates have expired")
		return errors.New("would prune all certificates")
	}

	if len(newBundle.JwtSigningKeys) == 0 {
		m.c.Log.Warn("Pruning halted; all known JWT signing keys have expired")
		return errors.New("would prune all JWT signing keys")
	}

	if changed {
		m.c.Metrics.IncrCounter([]string{"manager", "bundle", "pruned"}, 1)
		_, err := ds.UpdateBundle(ctx, &datastore.UpdateBundleRequest{
			Bundle: newBundle,
		})
		if err != nil {
			return fmt.Errorf("write new bundle: %v", err)
		}
	}

	return nil
}

func (m *Manager) appendBundle(ctx context.Context, caChain []*x509.Certificate, jwtSigningKey *common.PublicKey) error {
	var rootCAs []*common.Certificate
	for _, caCert := range caChain {
		rootCAs = append(rootCAs, &common.Certificate{
			DerBytes: caCert.Raw,
		})
	}

	var jwtSigningKeys []*common.PublicKey
	if jwtSigningKey != nil {
		jwtSigningKeys = append(jwtSigningKeys, jwtSigningKey)
	}

	ds := m.c.Catalog.GetDataStore()
	if _, err := ds.AppendBundle(ctx, &datastore.AppendBundleRequest{
		Bundle: &common.Bundle{
			TrustDomainId:  m.c.TrustDomain.String(),
			RootCas:        rootCAs,
			JwtSigningKeys: jwtSigningKeys,
		},
	}); err != nil {
		return err
	}

	return nil
}

func (m *Manager) loadJournal(ctx context.Context) error {
	jsonPath := filepath.Join(m.c.Dir, "certs.json")
	if ok, err := migrateJSONFile(jsonPath, m.journalPath()); err != nil {
		return errs.New("failed to migrate old JSON data: %v", err)
	} else if ok {
		m.c.Log.Info("Migrated data to journal")
	}

	// Load the journal and see if we can figure out the next and current
	// X509CA and JWTKey entries, if any.
	m.c.Log.WithField("path", m.journalPath()).Debug("Loading journal")

	journal, err := LoadJournal(m.journalPath())
	if err != nil {
		return err
	}

	m.journal = journal

	entries := journal.Entries()

	now := m.c.Clock.Now()

	m.c.Log.WithFields(logrus.Fields{
		"x509cas":  len(entries.X509CAs),
		"jwt_keys": len(entries.JwtKeys),
	}).Info("Journal loaded")

	if len(entries.X509CAs) > 0 {
		m.nextX509CA, err = m.tryLoadX509CASlotFromEntry(ctx, entries.X509CAs[len(entries.X509CAs)-1])
		if err != nil {
			return err
		}
		// if the last entry is ok, then consider the next entry
		if m.nextX509CA != nil && len(entries.X509CAs) > 1 {
			m.currentX509CA, err = m.tryLoadX509CASlotFromEntry(ctx, entries.X509CAs[len(entries.X509CAs)-2])
			if err != nil {
				return err
			}
		}
	}
	switch {
	case m.currentX509CA != nil:
		// both current and next are set
	case m.nextX509CA != nil:
		// next is set but not current. swap them and initialize next with an empty slot.
		m.currentX509CA, m.nextX509CA = m.nextX509CA, newX509CASlot(otherSlotID(m.nextX509CA.id))
	default:
		// neither are set. initialize them with empty slots.
		m.currentX509CA = newX509CASlot("A")
		m.nextX509CA = newX509CASlot("B")
	}

	if !m.currentX509CA.IsEmpty() && !m.currentX509CA.ShouldActivateNext(now) {
		// activate the X509CA immediately if it is set and not within
		// activation time of the next X509CA.
		m.activateX509CA()
	}

	if len(entries.JwtKeys) > 0 {
		m.nextJWTKey, err = m.tryLoadJWTKeySlotFromEntry(ctx, entries.JwtKeys[len(entries.JwtKeys)-1])
		if err != nil {
			return err
		}
		// if the last entry is ok, then consider the next entry
		if m.nextJWTKey != nil && len(entries.JwtKeys) > 1 {
			m.currentJWTKey, err = m.tryLoadJWTKeySlotFromEntry(ctx, entries.JwtKeys[len(entries.JwtKeys)-2])
			if err != nil {
				return err
			}
		}
	}
	switch {
	case m.currentJWTKey != nil:
		// both current and next are set
	case m.nextJWTKey != nil:
		// next is set but not current. swap them and initialize next with an empty slot.
		m.currentJWTKey, m.nextJWTKey = m.nextJWTKey, newJWTKeySlot(otherSlotID(m.nextJWTKey.id))
	default:
		// neither are set. initialize them with empty slots.
		m.currentJWTKey = newJWTKeySlot("A")
		m.nextJWTKey = newJWTKeySlot("B")
	}

	if !m.currentJWTKey.IsEmpty() && !m.currentJWTKey.ShouldActivateNext(now) {
		// activate the JWT key immediately if it is set and not within
		// activation time of the next JWT key.
		m.activateJWTKey()
	}

	return nil
}

func (m *Manager) journalPath() string {
	return filepath.Join(m.c.Dir, "journal.pem")
}

func (m *Manager) tryLoadX509CASlotFromEntry(ctx context.Context, entry *X509CAEntry) (*x509CASlot, error) {
	slot, badReason, err := m.loadX509CASlotFromEntry(ctx, entry)
	if err != nil {
		m.c.Log.WithFields(logrus.Fields{
			"slot_id": entry.SlotId,
			"error":   err.Error(),
		}).Error("X509CA slot failed to load")
		return nil, err
	}
	if badReason != "" {
		m.c.Log.WithFields(logrus.Fields{
			"slot_id": entry.SlotId,
			"reason":  badReason,
		}).Warn("X509CA slot unusable")
		return nil, nil
	}
	return slot, nil
}

func (m *Manager) loadX509CASlotFromEntry(ctx context.Context, entry *X509CAEntry) (*x509CASlot, string, error) {
	if entry.SlotId == "" {
		return nil, "no slot id", nil
	}

	chain := make([]*x509.Certificate, 0, len(entry.Chain))
	for _, certDER := range entry.Chain {
		cert, err := x509.ParseCertificate(certDER)
		if err != nil {
			return nil, "", errs.New("unable to parse chain: %v", err)
		}
		chain = append(chain, cert)
	}
	if len(chain) == 0 {
		return nil, "no certificates in chain", nil
	}

	signer, err := m.makeSigner(ctx, x509CAKmKeyId(entry.SlotId))
	if err != nil {
		return nil, "", err
	}

	switch {
	case signer == nil:
		return nil, "no key manager key", nil
	case !publicKeyEqual(chain[0].PublicKey, signer.Public()):
		return nil, "public key does not match key manager key", nil
	}

	return &x509CASlot{
		id:       entry.SlotId,
		issuedAt: time.Unix(entry.IssuedAt, 0),
		x509CA: &X509CA{
			Signer:         signer,
			Chain:          chain,
			IsIntermediate: entry.IsIntermediate,
		},
	}, "", nil
}

func (m *Manager) tryLoadJWTKeySlotFromEntry(ctx context.Context, entry *JWTKeyEntry) (*jwtKeySlot, error) {
	slot, badReason, err := m.loadJWTKeySlotFromEntry(ctx, entry)
	if err != nil {
		m.c.Log.WithFields(logrus.Fields{
			"slot_id": entry.SlotId,
			"error":   err.Error(),
		}).Error("JWT key slot failed to load")
		return nil, err
	}
	if badReason != "" {
		m.c.Log.WithFields(logrus.Fields{
			"slot_id": entry.SlotId,
			"reason":  badReason,
		}).Warn("JWT key slot unusable")
		return nil, nil
	}
	return slot, nil
}

func (m *Manager) loadJWTKeySlotFromEntry(ctx context.Context, entry *JWTKeyEntry) (*jwtKeySlot, string, error) {
	if entry.SlotId == "" {
		return nil, "no slot id", nil
	}

	publicKey, err := x509.ParsePKIXPublicKey(entry.PublicKey)
	if err != nil {
		return nil, "", errs.Wrap(err)
	}

	signer, err := m.makeSigner(ctx, jwtKeyKmKeyId(entry.SlotId))
	if err != nil {
		return nil, "", err
	}

	switch {
	case signer == nil:
		return nil, "no key manager key", nil
	case !publicKeyEqual(publicKey, signer.Public()):
		return nil, "public key does not match key manager key", nil
	}

	return &jwtKeySlot{
		id:       entry.SlotId,
		issuedAt: time.Unix(entry.IssuedAt, 0),
		jwtKey: &JWTKey{
			Signer:   signer,
			NotAfter: time.Unix(entry.NotAfter, 0),
			Kid:      entry.Kid,
		},
	}, "", nil
}

func (m *Manager) makeSigner(ctx context.Context, keyID string) (crypto.Signer, error) {
	km := m.c.Catalog.GetKeyManager()
	resp, err := km.GetPublicKey(ctx, &keymanager.GetPublicKeyRequest{
		KeyId: keyID,
	})
	if err != nil {
		return nil, errs.Wrap(err)
	}

	if resp.PublicKey == nil {
		return nil, nil
	}

	publicKey, err := x509.ParsePKIXPublicKey(resp.PublicKey.PkixData)
	if err != nil {
		return nil, errs.Wrap(err)
	}

	return cryptoutil.NewKeyManagerSigner(km, keyID, publicKey), nil
}

func x509CAKmKeyId(id string) string {
	return fmt.Sprintf("x509-CA-%s", id)
}

func jwtKeyKmKeyId(id string) string {
	return fmt.Sprintf("JWT-Signer-%s", id)
}

type x509CASlot struct {
	id       string
	issuedAt time.Time
	x509CA   *X509CA
}

func newX509CASlot(id string) *x509CASlot {
	return &x509CASlot{
		id: id,
	}
}

func (s *x509CASlot) KmKeyID() string {
	return x509CAKmKeyId(s.id)
}

func (s *x509CASlot) IsEmpty() bool {
	return s.x509CA == nil
}

func (s *x509CASlot) Reset() {
	s.x509CA = nil
}

func (s *x509CASlot) ShouldPrepareNext(now time.Time) bool {
	return s.x509CA != nil && now.After(preparationThreshold(s.issuedAt, s.x509CA.Chain[0].NotAfter))
}

func (s *x509CASlot) ShouldActivateNext(now time.Time) bool {
	return s.x509CA != nil && now.After(activationThreshold(s.issuedAt, s.x509CA.Chain[0].NotAfter))
}

type jwtKeySlot struct {
	id       string
	issuedAt time.Time
	jwtKey   *JWTKey
}

func newJWTKeySlot(id string) *jwtKeySlot {
	return &jwtKeySlot{
		id: id,
	}
}

func (s *jwtKeySlot) KmKeyID() string {
	return jwtKeyKmKeyId(s.id)
}

func (s *jwtKeySlot) IsEmpty() bool {
	return s.jwtKey == nil
}

func (s *jwtKeySlot) Reset() {
	s.jwtKey = nil
}

func (s *jwtKeySlot) ShouldPrepareNext(now time.Time) bool {
	return s.jwtKey == nil || now.After(preparationThreshold(s.issuedAt, s.jwtKey.NotAfter))
}

func (s *jwtKeySlot) ShouldActivateNext(now time.Time) bool {
	return s.jwtKey == nil || now.After(activationThreshold(s.issuedAt, s.jwtKey.NotAfter))
}

func otherSlotID(id string) string {
	if id == "A" {
		return "B"
	}
	return "A"
}

func publicKeyEqual(a, b crypto.PublicKey) bool {
	matches, err := cryptoutil.PublicKeyEqual(a, b)
	if err != nil {
		return false
	}
	return matches
}

func GenerateServerCACSR(signer crypto.Signer, trustDomain string, subject pkix.Name) ([]byte, error) {
	spiffeID := &url.URL{
		Scheme: "spiffe",
		Host:   trustDomain,
	}

	template := x509.CertificateRequest{
		Subject:            subject,
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		URIs:               []*url.URL{spiffeID},
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &template, signer)
	if err != nil {
		return nil, err
	}

	return csr, nil
}

func SignX509CA(ctx context.Context, signer crypto.Signer, upstreamCA upstreamca.UpstreamCA, upstreamBundle bool, trustDomain string, subject pkix.Name, notBefore, notAfter time.Time) (*X509CA, []*x509.Certificate, error) {
	// either self-sign or sign with the upstream CA
	var caChain []*x509.Certificate
	var trustBundle []*x509.Certificate
	var isIntermediate bool
	var err error
	if upstreamCA != nil {
		caChain, trustBundle, err = UpstreamSignServerCACertificate(ctx, upstreamCA, signer, trustDomain, subject)
		if err != nil {
			return nil, nil, err
		}
		isIntermediate = upstreamBundle
		if !isIntermediate {
			// we don't want to join the upstream PKI. Use the server CA as the
			// root, as if the upstreamCA was never configured.
			caChain = caChain[:1]
			trustBundle = caChain
		}
	} else {
		cert, err := SelfSignServerCACertificate(signer, trustDomain, subject, notBefore, notAfter)
		if err != nil {
			return nil, nil, err
		}
		caChain = []*x509.Certificate{cert}
		trustBundle = caChain
	}

	return &X509CA{
		Signer:         signer,
		Chain:          caChain,
		IsIntermediate: isIntermediate,
	}, trustBundle, nil
}

func UpstreamSignServerCACertificate(ctx context.Context, upstreamCA upstreamca.UpstreamCA, signer crypto.Signer, trustDomain string, subject pkix.Name) ([]*x509.Certificate, []*x509.Certificate, error) {
	csr, err := GenerateServerCACSR(signer, trustDomain, subject)
	if err != nil {
		return nil, nil, err
	}

	resp, err := upstreamCA.SubmitCSR(ctx, &upstreamca.SubmitCSRRequest{
		Csr: csr,
	})
	if err != nil {
		return nil, nil, err
	}

	return parseUpstreamCACSRResponse(resp)
}

func parseUpstreamCACSRResponse(resp *upstreamca.SubmitCSRResponse) ([]*x509.Certificate, []*x509.Certificate, error) {
	if resp.SignedCertificate != nil {
		certChain, err := x509.ParseCertificates(resp.SignedCertificate.CertChain)
		if err != nil {
			return nil, nil, err
		}
		trustBundle, err := x509.ParseCertificates(resp.SignedCertificate.Bundle)
		if err != nil {
			return nil, nil, err
		}
		return certChain, trustBundle, nil
	}

	// This is an old response from the upstream CA. The assumption from the
	// manager was that Cert contained a single certificate representing the
	// newly signed CA certificate and UpstreamTrustBundle contained the rest
	// of the full chain back to the upstream "root".
	cert, err := x509.ParseCertificate(resp.DEPRECATEDCert)
	if err != nil {
		return nil, nil, err
	}
	trustBundle, err := x509.ParseCertificates(resp.DEPRECATEDUpstreamTrustBundle)
	if err != nil {
		return nil, nil, err
	}

	certChain := []*x509.Certificate{cert}

	switch len(trustBundle) {
	case 0:
		return nil, nil, errors.New("upstream CA returned an empty trust bundle")
	case 1:
		return certChain, trustBundle, nil
	default:
		// append the "intermediates" at the start of the upstream bundle
		certChain = append(certChain, trustBundle[:len(trustBundle)-1]...)
		// only consider the "root" of the upstream bundle as part of the
		// trust bundle
		trustBundle = trustBundle[len(trustBundle)-1:]
		return certChain, trustBundle, nil
	}
	return certChain, trustBundle, nil
}

func SelfSignServerCACertificate(signer crypto.Signer, trustDomain string, subject pkix.Name, notBefore, notAfter time.Time) (*x509.Certificate, error) {
	csr, err := GenerateServerCACSR(signer, trustDomain, subject)
	if err != nil {
		return nil, err
	}

	template, err := CreateServerCATemplate(csr, trustDomain, notBefore, notAfter, big.NewInt(0))
	if err != nil {
		return nil, err
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, signer.Public(), signer)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

func preparationThreshold(issuedAt, notAfter time.Time) time.Time {
	lifetime := notAfter.Sub(issuedAt)
	return notAfter.Add(-lifetime / 2)
}

func activationThreshold(issuedAt, notAfter time.Time) time.Time {
	lifetime := notAfter.Sub(issuedAt)
	return notAfter.Add(-lifetime / 6)
}

func newJWTKey(signer crypto.Signer, expiresAt time.Time) (*JWTKey, error) {
	kid, err := newKeyID()
	if err != nil {
		return nil, err
	}

	return &JWTKey{
		Signer:   signer,
		Kid:      kid,
		NotAfter: expiresAt,
	}, nil
}

func publicKeyFromJWTKey(jwtKey *JWTKey) (*common.PublicKey, error) {
	pkixBytes, err := x509.MarshalPKIXPublicKey(jwtKey.Signer.Public())
	if err != nil {
		return nil, errs.Wrap(err)
	}

	return &common.PublicKey{
		PkixBytes: pkixBytes,
		Kid:       jwtKey.Kid,
		NotAfter:  jwtKey.NotAfter.Unix(),
	}, nil
}

func newKeyID() (string, error) {
	choices := make([]byte, 32)
	_, err := rand.Read(choices)
	if err != nil {
		return "", err
	}
	return keyIDFromBytes(choices), nil
}

func keyIDFromBytes(choices []byte) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := new(bytes.Buffer)
	for _, choice := range choices {
		buf.WriteByte(alphabet[int(choice)%len(alphabet)])
	}
	return buf.String()
}

func timeField(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
