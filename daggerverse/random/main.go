package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"

	"github.com/google/uuid"
)

type Random struct{}

// UuidV4 generates a random UUID version 4 and returns it as a string.
//
// +cache="never"
func (r *Random) UuidV4() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// UuidV7 generates a random UUID version 7 and returns it as a string.
//
// +cache="never"
func (r *Random) UuidV7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// Sha256 generates a random n-byte value and returns its SHA-256 hash as a hexadecimal string.
//
// +cache="never"
func (r *Random) Sha256(
	//+default=32
	n int,
) (string, error) {
	var b = make([]byte, n)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		return "", err
	}

	hash := sha256.Sum256(b[:])
	return hex.EncodeToString(hash[:]), nil
}

// Sha512 generates a random n-byte value and returns its SHA-512 hash as a hexadecimal string.
//
// +cache="never"
func (r *Random) Sha512(
	//+default=64
	n int,
) (string, error) {
	var b = make([]byte, n)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		return "", err
	}

	hash := sha512.Sum512(b[:])
	return hex.EncodeToString(hash[:]), nil
}
