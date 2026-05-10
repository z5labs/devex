package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// Random provides functions for generating random values such as UUIDs and
// random-derived SHA hashes. Each call returns a fresh value; results are not
// cached by the Dagger engine.
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
	if n <= 0 {
		return "", fmt.Errorf("n must be greater than 0, got %d", n)
	}

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
	if n <= 0 {
		return "", fmt.Errorf("n must be greater than 0, got %d", n)
	}

	var b = make([]byte, n)
	_, err := io.ReadFull(rand.Reader, b)
	if err != nil {
		return "", err
	}

	hash := sha512.Sum512(b[:])
	return hex.EncodeToString(hash[:]), nil
}

// Serial generates a random n-byte X.509 serial number and returns it as a
// lowercase hexadecimal string (n*2 chars). The default n=16 yields a 128-bit
// serial, which is the recommended size for newly issued certificates. The
// low bit is forced to 1 so the value parses as a positive big.Int regardless
// of consumer (RFC 5280 requires serials be positive integers).
//
// +cache="never"
func (r *Random) Serial(
	//+default=16
	n int,
) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("n must be greater than 0, got %d", n)
	}

	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	b[len(b)-1] |= 1
	return hex.EncodeToString(b), nil
}
