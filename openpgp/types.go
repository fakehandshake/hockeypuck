/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012, 2013  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package openpgp

import (
	"bytes"
	"code.google.com/p/go.crypto/openpgp/packet"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

type PacketVisitor func(PacketRecord) error

type PacketRecord interface {
	GetPacket() (packet.Packet, error)
	SetPacket(packet.Packet), error
	Visit(PacketVisitor) error
}

type Signable interface {
	AddSignature(*Signature)
}

// Model representing an OpenPGP public key packets.
// Searchable fields are extracted from the packet key material
// stored in Packet, for database indexing.
type Pubkey struct {
	RFingerprint   string    `db:"uuid"`
	Creation       time.Time `db:"creation"`
	Expiration     time.Time `db:"expiration"`
	State          int       `db:"state"`
	Packet         []byte    `db:"packet"`
	Ctime          time.Time `db:"ctime"`
	Mtime          time.Time `db:"mtime"`
	Md5            string    `db:"md5"`
	Sha256         string    `db:"sha256"`
	RevsigDigest   string    `db:"revsig_uuid"`
	Algorithm      int       `db:"algorithm"`
	BitLen         int       `db:"bit_len"`
	Signatures     []*Signature
	Subkeys        []*Subkey
	UserIds        []*UserId
	UserAttributes []*UserAttribute
	Revsig         *Signature
}

func (pubkey *Pubkey) Fingerprint() string {
	return Reverse(pubkey.RFingerprint)
}

func (pubkey *Pubkey) KeyId() string {
	return Reverse(pubkey.RFingerprint[:16])
}

func (pubkey *Pubkey) ShortId() string {
	return Reverse(pubkey.RFingerprint[:8])
}

func (pubkey *Pubkey) Serialize(w io.Writer) error {
	return w.Write(pubkey.Packet)
}

func (pubkey *Pubkey) GetPacket() (packet.Packet, error) {
	return pubkey.GetPublicKey()
}

func (pubkey *Pubkey) GetPublicKey() (*packet.PublicKey, error) {
	buf := bytes.NewBuffer(pubkey.Packet)
	pk, err := packet.Read(buf)
	return pk.(*packet.PublicKey), err
}

func (pubkey *Pubkey) SetPacket(p packet.Packet) error {
	pk, is := p.(*packet.PublicKey)
	if !is {
		return ErrInvalidPacketType
	}
	return pubkey.SetPublicKey(pk)
}

func (pubkey *Pubkey) SetPublicKey(pk *packet.PublicKey) error {
	buf := bytes.NewBuffer(nil)
	err = pk.Serialize(buf)
	if err != nil {
		return err
	}
	fingerprint := Fingerprint(pk)
	bitLen, err := pk.BitLength()
	if err != nil {
		return err
	}
	if pk.IsSubkey {
		log.Println("Expected primary public key packet, got sub-key")
		return InvalidPacketErr
	}
	pubkey.Packet = buf.Bytes()
	pubkey.RFingerprint = Reverse(fingerprint)
	pubkey.Creation = pk.CreationTime
	pubkey.Algorithm = int(pk.PubKeyAlgo)
	pubkey.BitLen = bitLen
	return nil
}

func (pubkey *Pubkey) Visit(visitor PacketVisitor) (err error) {
	err = visitor(pubkey)
	if err != nil {
		return
	}
	for _, sig := range pubkey.Signatures {
		err = sig.Visit(visitor)
		if err != nil {
			return
		}
	}
	for _, uid := range pubkey.UserIds {
		err = uid.Visit(visitor)
		if err != nil {
			return
		}
	}
	for _, uat := range pubkey.UserAttributes {
		err = uat.Visit(visitor)
		if err != nil {
			return
		}
	}
	for _, subkey := range pubkey.Subkeys {
		err = subkey.Visit(visitor)
		if err != nil {
			return
		}
	}
	return
}

type Signature struct {
	ScopedDigest       string    `db:"uuid"`
	Creation           time.Time `db:"creation"`
	Expiration         time.Time `db:"expiration"`
	State              int       `db:"state"`
	Packet             []byte    `db:"packet"`
	SigType            int       `db:"sig_type"`
	RIssuerKeyId       string    `db:"signer"`
	RIssuerFingerprint string    `db:"signer_uuid"`
	RevsigDigest       string    `db:"revsig_uuid"`
	Revsig             *Signature
}

func (sig *Signature) IssuerKeyId() string {
	return Reverse(sig.RIssuerKeyId)
}

func (sig *Signature) IssuerFingerprint() string {
	return Reverse(sig.RIssuerFingerprint)
}

func (sig *Signature) GetPacket() (packet.Packet, error) {
	return sig.GetSignature()
}

func (sig *Signature) GetSignature() (packet.Packet, error) {
	buf := bytes.NewBuffer(sig.Packet)
	return packet.Read(buf)
}

func (sig *Signature) SetPacket(p *packet.Packet) error {
	return sig.SetSignature(p)
}

func (sig *Signature) SetSignature(p *packet.Packet) error {
	switch s := p.(type) {
	case *packet.Signature:
		return sig.setPacketV4(s)
	}
	return InvalidPacketType
}

func (sig *Signature) setPacketV4(s *packet.Signature) error {
	buf := bytes.NewBuffer(nil)
	err = s.Serialize(buf)
	if err != nil {
		return err
	}
	if s.IssuerKeyId == nil {
		return errors.New("Signature missing issuer key ID")
	}
	sig.Creation = s.CreationTime
	sig.Packet = buf.Bytes()
	sig.SigType = s.SigType
	// Extract the issuer key id
	var issuerKeyId [8]byte
	binary.BigEndian.PutUint64(issuerKeyId[:], *s.IssuerKeyId)
	sigKeyId := hex.EncodeToString(issuerKeyId[:])
	sig.RIssuerKeyId = Reverse(sigKeyId)
	// Expiration time
	if s.SigLifetimeSecs != nil {
		sig.Expiration = s.CreationTime.Add(
			time.Duration(*s.SigLifetimeSecs) * time.Second).Unix()
	}
	return nil
}

func (sig *Signature) Visit(visitor PacketVisitor) (err error) {
	return visitor(sig)
}

type UserId struct {
	ScopedDigest  string    `db:"uuid"`
	Creation      time.Time `db:"creation"`
	Expiration    time.Time `db:"expiration"`
	State         int       `db:"state"`
	Packet        []byte    `db:"packet"`
	PubkeyRFP     string    `db:"pubkey_uuid"`
	Keywords      string    `db:"keywords"`
	SelfSignature *Signature
	Signatures    []*Signature
}

func (uid *UserId) GetPacket() (packet.Packet, error) {
	return uid.GetUserId()
}

func (uid *UserId) GetUserId() (*packet.UserId, error) {
	buf := bytes.NewBuffer(pubkey.Packet)
	u, err := packet.Read(buf)
	return u.(*packet.UserId), err
}

func (uid *UserId) SetPacket(p packet.Packet) error {
	u, is := p.(*packet.UserId)
	if !is {
		return ErrInvalidPacketType
	}
	return SetUserId(u)
}

func (uid *UserId) SetUserId(u *packet.UserId) error {
	buf := bytes.NewBuffer(nil)
	err = u.Serialize(buf)
	if err != nil {
		return err
	}
	uid.Packet = buf.Bytes()
	uid.Keywords = CleanUtf8(u.Id)
	return nil
}

func (uid *UserId) Visit(visitor PacketVisitor) (err error) {
	err = visitor(uid)
	if err != nil {
		return
	}
	for _, sig := range uid.Signatures {
		err = sig.Visit(visitor)
		if err != nil {
			return
		}
	}
	return
}

type UserAttribute struct {
	ScopedDigest  string    `db:"uuid"`
	Creation      time.Time `db:"creation"`
	Expiration    time.Time `db:"expiration"`
	State         int       `db:"state"`
	Packet        []byte    `db:"packet"`
	PubkeyRFP     string    `db:"pubkey_uuid"`
	SelfSignature *Signature
	Signatures    []*Signature
}

func (uat *UserAttribute) GetPacket() (packet.Packet, error) {
	return GetOpaquePacket()
}

func (uat *UserAttribute) GetOpaquePacket() (*packet.OpaquePacket, error) {
	buf := bytes.NewBuffer(uat.Packet)
	r := packet.NewOpaqueReader(buf)
	return r.Next()
}

func (uat *UserAttribute) SetPacket(p packet.Packet) error {
	op, is := p.(*packet.OpaquePacket)
	if !is {
		return ErrInvalidPacketType
	}
	return uat.SetOpaquePacket(op)
}

func (uat *UserAttribute) SetOpaquePacket(op *packet.OpaquePacket) error {
	buf := bytes.NewBuffer([]byte{})
	err := op.Serialize(buf)
	if err != nil {
		return err
	}
	uat.Packet = buf.Bytes()
}

// Image subpacket type
const ImageSubType = 1

// Byte offset of image data in image subpacket
const ImageSubOffset = 16

// Get all images contained in UserAttribute packet
func (uat *UserAttribute) GetJpegData() (result []*bytes.Buffer) {
	op, err := uat.GetPacket()
	if err != nil {
		return
	}
	subpackets, err := packet.OpaqueSubpackets(op.Contents)
	if err != nil {
		return
	}
	for _, subpacket := range subpackets {
		if subpacket.SubType == ImageSubType && len(subpacket.Contents) > ImageSubOffset {
			result = append(result,
				bytes.NewBuffer(subpacket.Contents[ImageSubOffset:]))
		}
	}
	return result
}

func (uat *UserAttribute) Visit(visitor PacketVisitor) (err error) {
	err = visitor(uat)
	if err != nil {
		return
	}
	for _, sig := range uat.Signatures {
		err = sig.Visit(visitor)
		if err != nil {
			return
		}
	}
	return
}

type Subkey struct {
	RFingerprint string    `db:"uuid"`
	Creation     time.Time `db:"creation"`
	Expiration   time.Time `db:"expiration"`
	State        int       `db:"state"`
	Packet       []byte    `db:"packet"`
	PubkeyRFP    string    `db:"pubkey_uuid"`
	RevsigDigest string    `db:"revsig_uuid"`
	Algorithm    int       `db:"algorithm"`
	BitLen       int       `db:"bit_len"`
	Signatures   []*Signatures
}

func (subkey *Subkey) Fingerprint() string {
	return Reverse(subkey.RFingerprint)
}

func (subkey *Subkey) KeyId() string {
	return Reverse(subkey.RFingerprint[:16])
}

func (subkey *Subkey) ShortId() string {
	return Reverse(subkey.RFingerprint[:8])
}

func (subkey *Subkey) GetPacket() (packet.Packet, error) {
	return subkey.GetPublicKey()
}

func (subkey *Subkey) GetPublicKey() (*packet.PublicKey, error) {
	buf := bytes.NewBuffer(subkey.GetPacket())
	pk, err := packet.Read(buf)
	return pk.(*packet.PublicKey), err
}

func (subkey *Subkey) SetPacket(p packet.Packet) error {
	pk, is := p.(*packet.PublicKey)
	if !is {
		return ErrInvalidPacketType
	}
	return subkey.SetPublicKey(pk)
}

func (subkey *Subkey) SetPublicKey(pk *packet.PublicKey) error {
	buf := bytes.NewBuffer(nil)
	err := pk.Serialize(buf)
	if err != nil {
		return err
	}
	fingerprint := Fingerprint(pk)
	bitLen, err := pk.BitLength()
	if err != nil {
		return err
	}
	if !pk.IsSubkey {
		log.Println("Expected sub-key packet, got primary public key")
		return InvalidPacketErr
	}
	subkey.Packet = buf.Bytes()
	subkey.RFingerprint = Reverse(fingerprint)
	subkey.Creation = pk.CreationTime
	subkey.Algorithm = int(pk.PubKeyAlgo)
	subkey.BitLen = bitLen
	return nil
}

func (subkey *Subkey) Visit(visitor PacketVisitor) (err error) {
	err = visitor(subkey)
	if err != nil {
		return
	}
	for _, sig := range subkey.Signatures {
		err = sig.Visit(visitor)
		if err != nil {
			return
		}
	}
	return
}

func (uid *UserId) SelfSignature() *Signature {
	for _, userSig := range uid.Signatures {
		if packet.SignatureType(userSig.SigType) == packet.SigTypePositiveCert {
			return userSig
		}
	}
	return nil
}

func (pk *Pubkey) SelfSignature() *Signature {
	for _, pkSig := range pk.Signatures {
		switch packet.SignatureType(pkSig.SigType) {
		case packet.SigTypePositiveCert:
			return pkSig
		case packet.SignatureType(0x19):
			return pkSig
		}
	}
	return nil
}

type packetSlice []*packet.OpaquePacket

func (ps packetSlice) Len() int {
	return len(ps)
}

func (ps packetSlice) Swap(i, j int) {
	ps[i], ps[j] = ps[j], ps[i]
}

type sksPacketSorter struct{ packetSlice }

func (sps sksPacketSorter) Less(i, j int) bool {
	cmp := int32(sps.packetSlice[i].Tag) - int32(sps.packetSlice[j].Tag)
	if cmp < 0 {
		return true
	} else if cmp > 0 {
		return false
	}
	return bytes.Compare(sps.packetSlice[i].Contents, sps.packetSlice[j].Contents) < 0
}

/* Appending signatures */

func (pubkey *Pubkey) AddSignature(sig *Signature) {
	pubkey.Signatures = append(pubkey.Signatures, sig)
}

func (uid *UserId) AddSignature(sig *Signature) {
	uid.Signatures = append(uid.Signatures, sig)
}

func (uat *UserAttribute) AddSignature(sig *Signature) {
	uat.Signatures = append(uat.Signatures, sig)
}

func (subkey *Subkey) AddSignature(sig *Signature) {
	subkey.Signatures = append(subkey.Signatures, sig)
}
