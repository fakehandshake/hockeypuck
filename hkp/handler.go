package hkp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/julienschmidt/httprouter"
	"golang.org/x/crypto/openpgp/armor"
	"gopkg.in/errgo.v1"
	"gopkg.in/hockeypuck/conflux.v2/recon"
	log "gopkg.in/hockeypuck/logrus.v0"
	"gopkg.in/statsd.v1"

	"github.com/hockeypuck/hockeypuck/openpgp"
	"github.com/hockeypuck/hockeypuck/util"
)

func httpError(w http.ResponseWriter, statusCode int, err error) {
	log.Errorf("HTTP %d: %v", statusCode, err)
	statsd.Increment(fmt.Sprintf("hkp.status.%d", statusCode), 1, 1)
	http.Error(w, http.StatusText(statusCode), statusCode)
}

type Handler struct {
	storage Storage
	peer    *SKSPeer
}

type HandlerOption func(h *Handler) error

func NewHandler(storage Storage, peer *SKSPeer) *Handler {
	return &Handler{
		storage: storage,
		peer:    peer,
	}
}

func (h *Handler) Register(r *httprouter.Router) {
	r.GET("/pks/lookup", h.Lookup)
	r.POST("/pks/add", h.Add)
	r.POST("/pks/hashquery", h.HashQuery)
}

func (h *Handler) Lookup(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	l, err := ParseLookup(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	switch l.Op {
	case OperationGet, OperationHGet:
		h.get(w, l)
	case OperationIndex, OperationVIndex:
		h.index(w, l)
	default:
		httpError(w, http.StatusNotFound, errgo.Newf("operation not found: %v", l.Op))
	}
}

func (h *Handler) HashQuery(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	hq, err := ParseHashQuery(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, errgo.Mask(err))
		return
	}
	var result []*openpgp.Pubkey
	for _, digest := range hq.Digests {
		rfps, err := h.storage.MatchMD5([]string{digest})
		if err != nil {
			log.Errorf("error resolving hashquery digest %q", digest)
			continue
		}
		keys, err := h.storage.FetchKeys(rfps)
		if err != nil {
			log.Errorf("error fetching hashquery key %q", digest)
			continue
		}
		result = append(result, keys...)
	}

	w.Header().Set("Content-Type", "pgp/keys")

	// Write the number of keys
	err = recon.WriteInt(w, len(result))
	for _, key := range result {
		// Write each key in binary packet format, prefixed with length
		err = writeHashqueryKey(w, key)
		if err != nil {
			log.Errorf("error writing hashquery key %q: %v", key.RFingerprint, err)
			return
		}
	}

	// SKS expects hashquery response to terminate with a CRLF
	_, err = w.Write([]byte{0x0d, 0x0a})
	if err != nil {
		log.Errorf("error writing hashquery terminator: %v", err)
	}
}

func writeHashqueryKey(w http.ResponseWriter, key *openpgp.Pubkey) error {
	var buf bytes.Buffer
	err := openpgp.WritePackets(&buf, key)
	if err != nil {
		return errgo.Mask(err)
	}
	err = recon.WriteInt(w, buf.Len())
	if err != nil {
		return errgo.Mask(err)
	}
	_, err = w.Write(buf.Bytes())
	if err != nil {
		return errgo.Mask(err)
	}
	return nil
}

func (h *Handler) resolve(l *Lookup) ([]string, error) {
	if l.Op == OperationHGet {
		return h.storage.MatchMD5([]string{l.Search})
	}
	if strings.HasPrefix(l.Search, "0x") {
		keyID := util.Reverse(strings.ToLower(l.Search[2:]))
		return h.storage.Resolve([]string{keyID})
	}
	return h.storage.MatchKeyword([]string{l.Search})
}

func (h *Handler) keys(l *Lookup) ([]*openpgp.Pubkey, error) {
	rfps, err := h.resolve(l)
	if err != nil {
		return nil, err
	}
	return h.storage.FetchKeys(rfps)
}

func (h *Handler) get(w http.ResponseWriter, l *Lookup) {
	keys, err := h.keys(l)
	if err != nil {
		httpError(w, http.StatusInternalServerError, errgo.Mask(err))
	}

	w.Header().Set("Content-Type", "text/plain")
	err = openpgp.WriteArmoredPackets(w, keys)
	if err != nil {
		log.Errorf("get %q: error writing armored keys: %v", l.Search, err)
	}
}

func (h *Handler) index(w http.ResponseWriter, l *Lookup) {
	keys, err := h.keys(l)
	if err != nil {
		httpError(w, http.StatusInternalServerError, errgo.Mask(err))
	}

	if l.Options[OptionMachineReadable] {
		h.indexMR(w, keys, l)
	} else {
		h.indexJSON(w, keys)
	}
}

func (h *Handler) indexJSON(w http.ResponseWriter, keys []*openpgp.Pubkey) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	err := enc.Encode(&keys)
	if err != nil {
		httpError(w, http.StatusInternalServerError, errgo.Mask(err))
	}
}

func mrTimeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d", t.Unix())
}

func (h *Handler) indexMR(w http.ResponseWriter, keys []*openpgp.Pubkey, l *Lookup) {
	w.Header().Set("Content-Type", "text/plain")

	fmt.Fprintln(w, "info:1:1")
	for _, key := range keys {
		selfsigs := key.SelfSigs()
		if !selfsigs.Valid() {
			continue
		}

		var keyID string
		if l.Fingerprint {
			keyID = key.Fingerprint()
		} else {
			keyID = key.KeyID()
		}
		keyID = strings.ToUpper(keyID)

		expiresAt, _ := selfsigs.ExpiresAt()

		fmt.Fprintln(w, "pub:%s:%d:%d:%d:%s:", keyID, key.Algorithm, key.BitLen,
			key.Creation.Unix(), mrTimeString(expiresAt))

		for _, uid := range key.UserIDs {
			selfsigs := uid.SelfSigs(key)
			validSince, ok := selfsigs.ValidSince()
			if !ok {
				continue
			}
			expiresAt, _ := selfsigs.ExpiresAt()
			fmt.Fprintf(w, "uid:%s:%d:%s:", strings.Replace(uid.Keywords, ":", "%3a", -1),
				validSince.Unix(), mrTimeString(expiresAt))
		}
	}
}

func (h *Handler) Add(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	add, err := ParseAdd(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, errgo.Mask(err))
		return
	}

	// Check and decode the armor
	armorBlock, err := armor.Decode(bytes.NewBufferString(add.Keytext))
	if err != nil {
		httpError(w, http.StatusBadRequest, errgo.Mask(err))
		return
	}

	var result struct {
		Inserted []string `json:"inserted"`
		Updated  []string `json:"updated"`
		Ignored  []string `json:"ignored"`
	}

	for readKey := range openpgp.ReadKeys(armorBlock.Body) {
		if readKey.Error != nil {
			httpError(w, http.StatusBadRequest, errgo.Mask(err))
			return
		}
		err := openpgp.DropDuplicates(readKey.Pubkey)
		if err != nil {
			httpError(w, http.StatusInternalServerError, errgo.Mask(err))
			return
		}
		change, err := UpsertKey(h.storage, readKey.Pubkey)
		if err != nil {
			httpError(w, http.StatusInternalServerError, errgo.Mask(err))
			return
		}

		h.peer.notifyKeyChange(change)

		fp := readKey.Pubkey.QualifiedFingerprint()
		switch change.(type) {
		case KeyAdded:
			result.Inserted = append(result.Inserted, fp)
		case KeyReplaced:
			result.Updated = append(result.Updated, fp)
		case KeyNotChanged:
			result.Ignored = append(result.Ignored, fp)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.Encode(&result)
}

type KeyChange interface {
	InsertDigests() []string
	RemoveDigests() []string
}

type KeyAdded struct {
	Digest string
}

func (ka KeyAdded) InsertDigests() []string {
	return []string{ka.Digest}
}

func (ka KeyAdded) RemoveDigests() []string {
	return nil
}

type KeyReplaced struct {
	OldDigest string
	NewDigest string
}

func (kr KeyReplaced) InsertDigests() []string {
	return []string{kr.NewDigest}
}

func (kr KeyReplaced) RemoveDigests() []string {
	return []string{kr.OldDigest}
}

type KeyNotChanged struct{}

func (knc KeyNotChanged) InsertDigests() []string { return nil }

func (knc KeyNotChanged) RemoveDigests() []string { return nil }

func UpsertKey(storage Storage, pubkey *openpgp.Pubkey) (KeyChange, error) {
	lastKeys, err := storage.FetchKeys([]string{pubkey.RFingerprint})
	if len(lastKeys) == 0 || IsNotFound(err) {
		err = storage.Insert([]*openpgp.Pubkey{pubkey})
		if err != nil {
			return nil, errgo.Mask(err)
		}
		return KeyAdded{Digest: pubkey.MD5}, nil
	}
	lastKey := lastKeys[0]
	lastMD5 := lastKey.MD5
	err = openpgp.Merge(lastKey, pubkey)
	if err != nil {
		return nil, errgo.Mask(err)
	}
	if lastMD5 != lastKey.MD5 {
		err = storage.Update(lastKey)
		if err != nil {
			return nil, errgo.Mask(err)
		}
		return KeyReplaced{OldDigest: lastMD5, NewDigest: lastKey.MD5}, nil
	}
	return KeyNotChanged{}, nil
}
