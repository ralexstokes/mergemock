package types

import "github.com/prysmaticlabs/prysm/shared/bls"

type HashTreeRoot interface {
	HashTreeRoot() ([32]byte, error)
}

func VerifySignature(obj HashTreeRoot, pk, s []byte) (bool, error) {
	msg, err := obj.HashTreeRoot()
	if err != nil {
		return false, err
	}
	sig, err := bls.SignatureFromBytes(s)
	if err != nil {
		return false, err
	}
	pubkey, err := bls.PublicKeyFromBytes(pk)
	if err != nil {
		return false, err
	}
	return sig.Verify(pubkey, msg[:]), nil
}
