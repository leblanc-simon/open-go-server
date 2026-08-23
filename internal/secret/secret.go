// Package secret implémente le keystore chiffré : génération, scellement et
// ouverture des secrets, protégés par une clé maître locale.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
)

// KeyLength est la taille de la clé maître (AES-256).
const KeyLength = 32

// DefaultPasswordLength est la longueur des mots de passe générés : 32
// caractères de l'alphabet ci-dessous, soit plus de 190 bits d'entropie.
const DefaultPasswordLength = 32

// alphabet exclut les caractères ambigus (O/0, l/1) et tous ceux qui posent
// problème dans un shell, une URL de connexion ou un fichier de configuration.
const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789-.+="

// Keystore scelle et ouvre les secrets.
type Keystore struct {
	aead cipher.AEAD
}

// ErrNoKey indique l'absence de clé maître.
var ErrNoKey = errors.New("clé maître absente")

// GenerateKey crée la clé maître et l'écrit avec des permissions restrictives.
// Le fichier est écrit en 0400 : sa perte rend tous les secrets irrécupérables,
// sa divulgation les expose tous.
func GenerateKey(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("la clé maître %s existe déjà : refus de l'écraser", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	key := make([]byte, KeyLength)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, key, 0o400); err != nil {
		return err
	}
	return nil
}

// Open charge la clé maître et prépare le keystore.
func Open(path string) (*Keystore, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w : %s (lancer « ogs init »)", ErrNoKey, path)
		}
		return nil, err
	}
	if len(key) != KeyLength {
		return nil, fmt.Errorf("clé maître %s de taille inattendue (%d octets, %d attendus)",
			path, len(key), KeyLength)
	}
	if info, err := os.Stat(path); err == nil {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("clé maître %s lisible au-delà de son propriétaire (mode %04o) : "+
				"corriger avec chmod 0400", path, perm)
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keystore{aead: aead}, nil
}

// aad lie un secret à son emplacement logique : un texte chiffré ne peut pas
// être déplacé d'un projet ou d'un usage à un autre.
func aad(scope, name string) []byte { return []byte(scope + "/" + name) }

// Seal chiffre un secret et renvoie le nonce et le texte chiffré.
func (k *Keystore) Seal(scope, name string, plaintext []byte) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	ciphertext = k.aead.Seal(nil, nonce, plaintext, aad(scope, name))
	return nonce, ciphertext, nil
}

// Open déchiffre un secret.
func (k *Keystore) Open(scope, name string, nonce, ciphertext []byte) ([]byte, error) {
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, aad(scope, name))
	if err != nil {
		return nil, fmt.Errorf("déchiffrement de %s/%s impossible : clé maître différente "+
			"ou données altérées", scope, name)
	}
	return plaintext, nil
}

// Password génère un mot de passe aléatoire.
func Password(length int) (string, error) {
	if length <= 0 {
		length = DefaultPasswordLength
	}
	out := make([]byte, length)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}
