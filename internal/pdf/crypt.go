package pdf

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha256"
	"crypto/sha512"
	"hash"
)

// Herald decrypts only documents that open without a password.
//
// Most "encrypted" academic PDFs are encrypted with an empty user password and
// a permissions flag — the file opens in any reader without a prompt, and the
// encryption exists to express printing or copying restrictions. Reading text
// out of one of those is the same act as opening it. A file that genuinely
// needs a password is reported as such and never guessed at: Herald does not
// try passwords, and does not attempt to defeat a document it cannot open.

// padding is the fixed 32-byte string the standard security handler appends to
// a user password before hashing it.
var padding = []byte{
	0x28, 0xBF, 0x4E, 0x5E, 0x4E, 0x75, 0x8A, 0x41, 0x64, 0x00, 0x4E, 0x56,
	0xFF, 0xFA, 0x01, 0x08, 0x2E, 0x2E, 0x00, 0xB6, 0xD0, 0x68, 0x3E, 0x80,
	0x2F, 0x0C, 0xA9, 0xFE, 0x64, 0x53, 0x69, 0x7A,
}

// cipherKind is the algorithm a crypt filter selects.
type cipherKind int

const (
	cipherNone cipherKind = iota
	cipherRC4
	cipherAESV2 // AES-128
	cipherAESV3 // AES-256
)

type decryptor struct {
	key          []byte
	streamCipher cipherKind
	stringCipher cipherKind
	encryptRef   int // object number of the /Encrypt dictionary, never encrypted
}

// setupDecryption prepares object decryption when the trailer declares it.
func (r *Reader) setupDecryption() error {
	encryptValue, present := r.trailer["Encrypt"]
	if !present || encryptValue == nil {
		return nil
	}
	encryptRef := 0
	if ref, ok := encryptValue.(Ref); ok {
		encryptRef = ref.Number
	}
	encrypt, ok := r.GetDict(encryptValue)
	if !ok {
		return errorf("The PDF declares encryption Herald cannot read")
	}
	if filter, _ := r.GetName(encrypt["Filter"]); filter != "Standard" {
		return errorf("The PDF uses a security handler Herald cannot read")
	}

	version, _ := r.GetInt(encrypt["V"])
	revision, _ := r.GetInt(encrypt["R"])
	keyBits := 40
	if value, ok := r.GetInt(encrypt["Length"]); ok && value >= 40 && value <= 256 {
		keyBits = value
	}

	streamCipher, stringCipher := cipherRC4, cipherRC4
	if version >= 4 {
		streamCipher = r.cryptFilter(encrypt, "StmF", &keyBits)
		stringCipher = r.cryptFilter(encrypt, "StrF", &keyBits)
	}
	if version == 5 {
		streamCipher, stringCipher = cipherAESV3, cipherAESV3
		keyBits = 256
	}

	ownerHash := stringBytes(r.Resolve(encrypt["O"]))
	userHash := stringBytes(r.Resolve(encrypt["U"]))
	permissions, _ := r.GetInt(encrypt["P"])
	encryptMetadata := true
	if value, ok := r.Resolve(encrypt["EncryptMetadata"]).(Bool); ok {
		encryptMetadata = bool(value)
	}

	var key []byte
	var err error
	if revision >= 5 {
		key, err = r.aes256Key(encrypt, userHash, revision)
	} else {
		key = r.legacyKey(userHash, ownerHash, permissions, keyBits, revision, encryptMetadata)
	}
	if err != nil {
		return err
	}
	if key == nil {
		return errorf("The PDF is password protected")
	}
	r.decryptor = &decryptor{
		key: key, streamCipher: streamCipher,
		stringCipher: stringCipher, encryptRef: encryptRef,
	}
	// Objects cached before the key existed were parsed undecrypted.
	clear(r.cache)
	clear(r.objStreams)
	return nil
}

// cryptFilter reads the algorithm named by /StmF or /StrF out of /CF.
func (r *Reader) cryptFilter(encrypt Dict, key Name, keyBits *int) cipherKind {
	name, ok := r.GetName(encrypt[key])
	if !ok || name == "Identity" {
		return cipherNone
	}
	filters, ok := r.GetDict(encrypt["CF"])
	if !ok {
		return cipherRC4
	}
	filter, ok := r.GetDict(filters[name])
	if !ok {
		return cipherRC4
	}
	if length, ok := r.GetInt(filter["Length"]); ok && length > 0 {
		// /Length here is in bytes in most producers but in bits in some.
		if length <= 32 {
			length *= 8
		}
		if length >= 40 && length <= 256 {
			*keyBits = length
		}
	}
	switch method, _ := r.GetName(filter["CFM"]); method {
	case "AESV2":
		return cipherAESV2
	case "AESV3":
		return cipherAESV3
	case "None":
		return cipherNone
	default:
		return cipherRC4
	}
}

// legacyKey derives the file key for revisions 2 to 4 from an empty user
// password. It returns nil when the document needs a real password.
func (r *Reader) legacyKey(userHash, ownerHash []byte, permissions, keyBits, revision int, encryptMetadata bool) []byte {
	keyLength := keyBits / 8
	if keyLength < 5 {
		keyLength = 5
	}
	if keyLength > 16 {
		keyLength = 16
	}

	digest := md5.New()
	digest.Write(padding)
	if len(ownerHash) >= 32 {
		digest.Write(ownerHash[:32])
	} else {
		digest.Write(ownerHash)
	}
	value := uint32(int32(permissions))
	digest.Write([]byte{byte(value), byte(value >> 8), byte(value >> 16), byte(value >> 24)})
	digest.Write(r.firstFileID())
	if revision >= 4 && !encryptMetadata {
		digest.Write([]byte{0xff, 0xff, 0xff, 0xff})
	}
	key := digest.Sum(nil)

	if revision >= 3 {
		for range 50 {
			sum := md5.Sum(key[:keyLength])
			key = sum[:]
		}
	}
	key = key[:keyLength]

	if !validLegacyKey(key, userHash, revision, r.firstFileID()) {
		return nil
	}
	return key
}

// validLegacyKey checks the derived key against /U the way a reader does when
// deciding whether the empty user password opens the document.
func validLegacyKey(key, userHash []byte, revision int, fileID []byte) bool {
	if len(userHash) < 16 {
		// Without a usable /U there is nothing to check against; trusting the
		// derived key is better than refusing a file that may well open.
		return true
	}
	if revision == 2 {
		cipherStream, err := rc4.NewCipher(key)
		if err != nil {
			return false
		}
		out := make([]byte, 32)
		cipherStream.XORKeyStream(out, padding)
		return equalBytes(out, userHash[:32], 32)
	}

	digest := md5.New()
	digest.Write(padding)
	digest.Write(fileID)
	expected := digest.Sum(nil)

	out := make([]byte, 16)
	cipherStream, err := rc4.NewCipher(key)
	if err != nil {
		return false
	}
	cipherStream.XORKeyStream(out, expected)
	for round := 1; round <= 19; round++ {
		rotated := make([]byte, len(key))
		for index := range key {
			rotated[index] = key[index] ^ byte(round)
		}
		cipherStream, err := rc4.NewCipher(rotated)
		if err != nil {
			return false
		}
		cipherStream.XORKeyStream(out, out)
	}
	// Only the first 16 bytes are defined; the rest of /U is arbitrary padding.
	return equalBytes(out, userHash[:16], 16)
}

func equalBytes(left, right []byte, count int) bool {
	if len(left) < count || len(right) < count {
		return false
	}
	for index := range count {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// aes256Key derives the AES-256 file key for revisions 5 and 6.
func (r *Reader) aes256Key(encrypt Dict, userHash []byte, revision int) ([]byte, error) {
	if len(userHash) < 48 {
		return nil, errorf("The PDF's encryption dictionary is incomplete")
	}
	validationSalt, keySalt := userHash[32:40], userHash[40:48]

	// An empty user password must reproduce the stored hash; if it does not,
	// the document genuinely requires a password.
	if !equalBytes(revisionHash(nil, validationSalt, nil, revision), userHash[:32], 32) {
		return nil, errorf("The PDF is password protected")
	}

	intermediate := revisionHash(nil, keySalt, nil, revision)
	encryptedKey := stringBytes(r.Resolve(encrypt["UE"]))
	if len(encryptedKey) < 32 {
		return nil, errorf("The PDF's encryption dictionary is incomplete")
	}
	block, err := aes.NewCipher(intermediate)
	if err != nil {
		return nil, errorf("The PDF uses an encryption key Herald cannot read")
	}
	fileKey := make([]byte, 32)
	// The file key is stored CBC-encrypted with a zero initialization vector
	// and no padding.
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(fileKey, encryptedKey[:32])
	return fileKey, nil
}

// revisionHash is SHA-256 for revision 5 and the iterated hash of algorithm
// 2.B for revision 6.
func revisionHash(password, salt, userData []byte, revision int) []byte {
	initial := sha256.New()
	initial.Write(password)
	initial.Write(salt)
	initial.Write(userData)
	key := initial.Sum(nil)
	if revision < 6 {
		return key
	}

	var input []byte
	for round := 0; ; round++ {
		input = input[:0]
		input = append(input, password...)
		input = append(input, key...)
		input = append(input, userData...)

		repeated := make([]byte, 0, len(input)*64)
		for range 64 {
			repeated = append(repeated, input...)
		}
		block, err := aes.NewCipher(key[:16])
		if err != nil {
			return key[:32]
		}
		encrypted := make([]byte, len(repeated)-len(repeated)%aes.BlockSize)
		cipher.NewCBCEncrypter(block, key[16:32]).CryptBlocks(encrypted, repeated[:len(encrypted)])

		total := 0
		for _, b := range encrypted[:min(16, len(encrypted))] {
			total += int(b)
		}
		var next hash.Hash
		switch total % 3 {
		case 0:
			next = sha256.New()
		case 1:
			next = sha512.New384()
		default:
			next = sha512.New()
		}
		next.Write(encrypted)
		key = next.Sum(nil)

		// The loop runs at least 64 times and then until the last encrypted
		// byte falls below the round number.
		if round >= 63 && len(encrypted) > 0 && int(encrypted[len(encrypted)-1]) <= round-31 {
			return key[:32]
		}
		if round > 512 {
			// Defensive: a corrupt file must not spin here forever.
			return key[:32]
		}
	}
}

func (r *Reader) firstFileID() []byte {
	ids, ok := r.GetArray(r.trailer["ID"])
	if !ok || len(ids) == 0 {
		return nil
	}
	return stringBytes(r.Resolve(ids[0]))
}

func stringBytes(object Object) []byte {
	if value, ok := object.(String); ok {
		return []byte(value)
	}
	return nil
}

// objectKey derives the per-object key. AES-256 uses the file key directly;
// every earlier revision mixes in the object and generation numbers.
func (d *decryptor) objectKey(number, generation int, kind cipherKind) []byte {
	if kind == cipherAESV3 {
		return d.key
	}
	digest := md5.New()
	digest.Write(d.key)
	digest.Write([]byte{byte(number), byte(number >> 8), byte(number >> 16)})
	digest.Write([]byte{byte(generation), byte(generation >> 8)})
	if kind == cipherAESV2 {
		digest.Write([]byte{0x73, 0x41, 0x6c, 0x54}) // "sAlT"
	}
	key := digest.Sum(nil)
	length := min(len(d.key)+5, 16)
	return key[:length]
}

// decrypt decodes a stream body.
func (d *decryptor) decrypt(data []byte, number, generation int) []byte {
	return d.apply(data, number, generation, d.streamCipher)
}

// decryptString decodes a string object.
func (d *decryptor) decryptString(data []byte, number, generation int) []byte {
	return d.apply(data, number, generation, d.stringCipher)
}

func (d *decryptor) apply(data []byte, number, generation int, kind cipherKind) []byte {
	if kind == cipherNone || len(data) == 0 || number == 0 {
		return data
	}
	key := d.objectKey(number, generation, kind)
	switch kind {
	case cipherRC4:
		stream, err := rc4.NewCipher(key)
		if err != nil {
			return data
		}
		out := make([]byte, len(data))
		stream.XORKeyStream(out, data)
		return out
	case cipherAESV2, cipherAESV3:
		block, err := aes.NewCipher(key)
		if err != nil || len(data) <= aes.BlockSize {
			return data
		}
		body := data[aes.BlockSize:]
		body = body[:len(body)-len(body)%aes.BlockSize]
		if len(body) == 0 {
			return nil
		}
		out := make([]byte, len(body))
		cipher.NewCBCDecrypter(block, data[:aes.BlockSize]).CryptBlocks(out, body)
		// Remove PKCS#7 padding, ignoring a malformed final block.
		if pad := int(out[len(out)-1]); pad >= 1 && pad <= aes.BlockSize && pad <= len(out) {
			out = out[:len(out)-pad]
		}
		return out
	}
	return data
}

// skipStream reports the streams that are never encrypted.
func (d *decryptor) skipStream(r *Reader, dict Dict) bool {
	if r.currentNumber == d.encryptRef {
		return true
	}
	// A cross-reference stream must be readable before the key is known, so it
	// is defined to be unencrypted.
	if kind, ok := r.GetName(dict["Type"]); ok && kind == "XRef" {
		return true
	}
	return false
}

// decryptObject rewrites the strings inside a freshly parsed indirect object.
func (r *Reader) decryptObject(object Object, depth int) Object {
	if r.decryptor == nil || depth > maxNesting || r.currentNumber == r.decryptor.encryptRef {
		return object
	}
	switch value := object.(type) {
	case String:
		return String(r.decryptor.decryptString([]byte(value), r.currentNumber, r.currentGen))
	case Array:
		for index, item := range value {
			value[index] = r.decryptObject(item, depth+1)
		}
		return value
	case Dict:
		for key, item := range value {
			value[key] = r.decryptObject(item, depth+1)
		}
		return value
	}
	return object
}
