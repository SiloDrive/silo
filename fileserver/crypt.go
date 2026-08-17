package silod

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

type seafileCrypt struct {
	key     []byte
	iv      []byte
	version int
}

func (crypt *seafileCrypt) encrypt(input []byte) ([]byte, error) {
	key := crypt.key
	if crypt.version == 3 {
		key = to16Bytes(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	size := block.BlockSize()
	input = pkcs7Padding(input, size)
	out := make([]byte, len(input))

	if crypt.version == 3 {
		for bs, be := 0, size; bs < len(input); bs, be = bs+size, be+size {
			block.Encrypt(out[bs:be], input[bs:be])
		}
		return out, nil
	}

	blockMode := cipher.NewCBCEncrypter(block, crypt.iv)
	blockMode.CryptBlocks(out, input)

	return out, nil
}

func (crypt *seafileCrypt) decrypt(input []byte) ([]byte, error) {
	key := crypt.key
	if crypt.version == 3 {
		key = to16Bytes(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(input))
	size := block.BlockSize()

	// Checked before either mode runs. The ECB loop slices out[bs:be] past
	// the end of a short final block, and CryptBlocks panics outright on a
	// length that is not a whole number of blocks. The input is a stored
	// object, so a truncated or corrupt one would take the fileserver down
	// rather than fail the one request that touched it.
	if len(input) == 0 || len(input)%size != 0 {
		return nil, fmt.Errorf("encrypted data is %d bytes, not a whole number of %d-byte blocks",
			len(input), size)
	}

	if crypt.version == 3 {
		// Encryption repo v3 uses AES_128_ecb mode to encrypt and decrypt, each block is encrypted and decrypted independently,
		// there is no relationship before and after, and iv is not required.
		for bs, be := 0, size; bs < len(input); bs, be = bs+size, be+size {
			block.Decrypt(out[bs:be], input[bs:be])
		}
		return pkcs7UnPadding(out, size)
	}

	blockMode := cipher.NewCBCDecrypter(block, crypt.iv)
	blockMode.CryptBlocks(out, input)

	return pkcs7UnPadding(out, size)
}

func pkcs7Padding(p []byte, blockSize int) []byte {
	padding := blockSize - len(p)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(p, padtext...)
}

// pkcs7UnPadding strips PKCS#7 padding, validating it rather than trusting
// it.
//
// What it is handed is the result of decrypting with a key that may be the
// wrong one, in which case the "plaintext" is noise and its last byte is an
// arbitrary number. Reading p[length-1] panicked on an empty slice, and a
// padding length larger than the buffer sliced out of range — a panic in a
// request goroutine takes the whole fileserver with it, not just that
// request.
func pkcs7UnPadding(p []byte, blockSize int) ([]byte, error) {
	length := len(p)
	if length == 0 || length%blockSize != 0 {
		return nil, fmt.Errorf("decrypted data is %d bytes, not a whole number of %d-byte blocks",
			length, blockSize)
	}

	padLen := int(p[length-1])
	if padLen == 0 || padLen > blockSize || padLen > length {
		return nil, fmt.Errorf("invalid padding")
	}
	// Every padding byte carries the padding length; anything else means this
	// was not the plaintext we produced.
	for _, b := range p[length-padLen:] {
		if int(b) != padLen {
			return nil, fmt.Errorf("invalid padding")
		}
	}

	return p[:length-padLen], nil
}

func to16Bytes(input []byte) []byte {
	out := make([]byte, 16)
	copy(out, input)

	return out
}
