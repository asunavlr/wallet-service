package auth_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
)

// assinarRS256 produz a assinatura de um JWT. Vive num arquivo próprio para
// deixar claro que é infraestrutura do teste, e não o que está sob teste.
func assinarRS256(chave *rsa.PrivateKey, corpo string) (string, error) {
	soma := sha256.Sum256([]byte(corpo))
	assinatura, err := rsa.SignPKCS1v15(rand.Reader, chave, crypto.SHA256, soma[:])
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(assinatura), nil
}
