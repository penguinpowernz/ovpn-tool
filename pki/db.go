// db.go - db storage for certs, server info etc.
//
// (c) 2018 Sudhi Herle; License GPLv2
//
// This software does not come with any express or implied
// warranty; it is provided "as is". No claim  is made to its
// suitability for any purpose.

package pki

// Internal details:
//
// * All data written to the db is encrypted with a key derived from a
//   random 32-byte key.
// * This DB key is stored in an encrypted form in the DB; it is encrypted
//   with a user supplied passphrase:
//     dbkey = randbytes(32)
//     expanded = SHA512(passphrase)
//     kek = KDF(expanded, salt)
//     esk = kek ^ dbkey
//
// * Updating serial#: anytime a user cert or a server cert is written,
//   we update the serial number at the same time. We also update serial
//   number when CA is created for the first time.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"crypto/x509"
	"encoding/gob"
	"encoding/pem"
	"fmt"
	bolt "github.com/etcd-io/bbolt"
	"math/big"
	"os"
	"path"
	"time"
)

type database struct {
	db   *bolt.DB
	pwd  []byte // expanded 64 byte passphrase
	salt []byte // KDF salt

	// set to true if CA has been initialized
	initialized bool
}

type cadata struct {
	Cert
	serial *big.Int
}

// gob encoded Cert pair
type certgob struct {
	Cert []byte
	Key  []byte

	Additional []byte
}

func newDB(fn string, pw string, creat bool) (*database, error) {
	fi, _ := os.Stat(fn)
	if fi != nil {
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s: not a regular file", fn)
		}
	} else if !creat {
		return nil, fmt.Errorf("can't open DB %s", fn)
	}

	dbdir := path.Dir(fn)
	err := os.MkdirAll(dbdir, 0700)
	if err != nil {
		return nil, fmt.Errorf("can't create dir %s for DB %s: %s", dbdir, fn, err)
	}

	db, err := bolt.Open(fn, 0600, nil)
	if err != nil {
		return nil, err
	}

	h := sha512.New()
	h.Write([]byte(pw))
	expanded := h.Sum(nil)

	var salt []byte

	pwd := make([]byte, 32)

	// initialize key buckets
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte("server"))
		if err != nil {
			return fmt.Errorf("%s: can't create server bucket: %s", fn, err)
		}

		_, err = tx.CreateBucketIfNotExists([]byte("user"))
		if err != nil {
			return fmt.Errorf("%s: can't create user bucket: %s", fn, err)
		}

		_, err = tx.CreateBucketIfNotExists([]byte("revoked"))
		if err != nil {
			return fmt.Errorf("%s: can't create revoked bucket: %s", fn, err)
		}

		b, err := tx.CreateBucketIfNotExists([]byte("config"))
		if err != nil {
			return fmt.Errorf("%s: can't create ca bucket: %s", fn, err)
		}

		skey := []byte("salt")
		ckey := []byte("check")
		pkey := []byte("ekey")

		salt = b.Get(skey)
		chk := b.Get(ckey)
		ekey := b.Get(pkey)
		if salt == nil || chk == nil || ekey == nil ||
			len(ekey) != 32 || len(chk) != 32 || len(salt) != 32 {

			salt = make([]byte, 32)

			// generate a random DB key and encrypt it with the user supplied key
			randsalt(pwd)

			// initialize the DB salt and derive a KEK
			randsalt(salt)
			kek := kdf(expanded, salt)

			var ekey [32]byte
			for i := 0; i < 32; i++ {
				ekey[i] = kek[i] ^ pwd[i]
			}

			h := sha256.New()
			h.Write(salt)
			h.Write(kek)
			chk = h.Sum(nil)

			b.Put(skey, salt)
			b.Put(ckey, chk)
			b.Put(pkey, ekey[:])

			return nil
		}

		// This may be an initialized DB. Lets verify it.
		kek := kdf(expanded, salt)

		h := sha256.New()
		h.Write(salt)
		h.Write(kek)
		vrfy := h.Sum(nil)

		if subtle.ConstantTimeCompare(chk, vrfy) != 1 {
			return fmt.Errorf("%s: wrong password", fn)
		}

		// finally decode the encrypted DB key
		for i := 0; i < 32; i++ {
			pwd[i] = ekey[i] ^ kek[i]
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	d := &database{
		db:   db,
		pwd:  pwd,
		salt: salt,
	}

	return d, nil
}

func (d *database) close() error {
	// wipe the keys
	for i := 0; i < 32; i++ {
		d.pwd[i] = 0
	}
	return d.db.Close()
}

// Rekey a database with a new user supplied password
func (d *database) Rekey(newpw string) error {
	h := sha512.New()
	h.Write([]byte(newpw))
	newpwd := h.Sum(nil)

	// initialize key buckets
	err := d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("config"))
		if b == nil {
			return fmt.Errorf("can't find config bucket")
		}

		ckey := []byte("check")
		pkey := []byte("ekey")

		// New KEK
		kek := kdf(newpwd, d.salt)

		h := sha256.New()
		h.Write(d.salt)
		h.Write(kek)
		chk := h.Sum(nil)

		var ekey [32]byte
		for i := 0; i < 32; i++ {
			ekey[i] = kek[i] ^ d.pwd[i]
		}

		b.Put(ckey, chk)
		b.Put(pkey, ekey[:])
		return nil
	})
	return err
}

// Return an initialized ca info
func (d *database) getCA() (*cadata, error) {
	var c *cadata

	err := d.db.View(func(tx *bolt.Tx) error {
		bc := tx.Bucket([]byte("config"))
		if bc == nil {
			return fmt.Errorf("can't find config bucket")
		}

		rgb := bc.Get(d.key("ca"))
		rsn := bc.Get(d.key("serial"))
		if rgb == nil || rsn == nil {
			return nil
		}

		gb, err := d.decrypt(rgb)
		if err != nil {
			return fmt.Errorf("can't decrypt ca: %s", err)
		}

		sn, err := d.decrypt(rsn)
		if err != nil {
			return fmt.Errorf("can't decrypt serial#: %s", err)
		}

		ck, err := decodeCert("ca", gb)
		if err != nil {
			return err
		}

		pw := fmt.Sprintf("%x", d.pwd)
		err = ck.decryptKey(ck.Rawkey, pw)
		if err != nil {
			return err
		}

		c = &cadata{
			Cert:   *ck,
			serial: big.NewInt(0).SetBytes(sn),
		}

		d.initialized = true
		return nil
	})

	return c, err
}

// Decode a serialized cert/key pair
func decodeCert(cn string, ub []byte) (*Cert, error) {
	var cg certgob

	b := bytes.NewBuffer(ub)
	g := gob.NewDecoder(b)
	err := g.Decode(&cg)
	if err != nil {
		return nil, fmt.Errorf("%s: can't decode gob: %s", cn, err)
	}
	cert, err := x509.ParseCertificate(cg.Cert)
	if err != nil {
		return nil, fmt.Errorf("%s: can't parse cert: %s", cn, err)
	}

	ck := &Cert{
		Crt:        cert,
		Rawkey:     cg.Key,
		Additional: cg.Additional,
	}
	return ck, nil
}

// Given a Cert, a raw key block and a password, decrypt the privatekey
// and set it to c.Key
func (c *Cert) decryptKey(key []byte, pw string) error {
	blk, _ := pem.Decode(key)

	var der []byte = blk.Bytes
	var err error

	if x509.IsEncryptedPEMBlock(blk) {
		pass := []byte(pw)
		der, err = x509.DecryptPEMBlock(blk, pass)
		if err != nil {
			return fmt.Errorf("can't decrypt private key (pw=%s): %s", pw, err)
		}
	}

	sk, err := x509.ParseECPrivateKey(der)
	if err == nil {
		c.Key = sk
	}

	return err
}

// given a Cert, marshal the private key and return as bytes
func (c *Cert) encryptKey(pw string) ([]byte, error) {
	if c.Key == nil {
		return nil, fmt.Errorf("privatkey is nil")
	}

	derkey, err := x509.MarshalECPrivateKey(c.Key)
	if err != nil {
		return nil, fmt.Errorf("can't marshal private key: %s", err)
	}

	var blk *pem.Block
	if len(pw) > 0 {
		pass := []byte(pw)
		blk, err = x509.EncryptPEMBlock(rand.Reader, "EC PRIVATE KEY", derkey, pass, x509.PEMCipherAES256)
		if err != nil {
			return nil, err
		}
	} else {
		blk = &pem.Block{
			Type:  "EC PRIVATE KEY",
			Bytes: derkey,
		}
	}

	return pem.EncodeToMemory(blk), nil
}

// marshal a Cert into a gob stream
func (c *Cert) marshal(pw string) ([]byte, error) {
	sn := c.Crt.Subject.CommonName
	if c.Crt.Raw == nil {
		return nil, fmt.Errorf("%s: Raw cert is nil?", sn)
	}

	key, err := c.encryptKey(pw)
	if err != nil {
		return nil, err
	}

	cg := &certgob{
		Cert:       c.Crt.Raw,
		Key:        key,
		Additional: c.Additional,
	}

	var b bytes.Buffer
	g := gob.NewEncoder(&b)
	err = g.Encode(cg)
	if err != nil {
		return nil, fmt.Errorf("%s: can't gob-encode cert: %s", sn, err)
	}

	return b.Bytes(), nil
}

// marshal and write the CA to disk
// Also update the serial#
func (d *database) putCA(ca *cadata) error {
	if d.initialized {
		return fmt.Errorf("CA already initialized")
	}

	pw := fmt.Sprintf("%x", d.pwd)
	b, err := ca.Cert.marshal(pw)
	if err != nil {
		return err
	}

	eb, err := d.encrypt(b)
	if err != nil {
		return fmt.Errorf("can't encrypt ca: %s", err)
	}

	es, err := d.encrypt(ca.Crt.SerialNumber.Bytes())
	if err != nil {
		return fmt.Errorf("can't encrypt serial#: %s", err)
	}

	err = d.db.Update(func(tx *bolt.Tx) error {
		bc := tx.Bucket([]byte("config"))
		if bc == nil {
			return fmt.Errorf("can't find config bucket")
		}

		err := bc.Put(d.key("ca"), eb)
		if err != nil {
			return fmt.Errorf("can't write ca data: %s", err)
		}

		err = bc.Put(d.key("serial"), es)
		if err != nil {
			return fmt.Errorf("can't write serial#: %s", err)
		}
		return nil
	})

	return err
}

// return a server or user cert
func (d *database) getcert(cn string, table string) (*Cert, error) {
	var c *Cert

	err := d.db.View(func(tx *bolt.Tx) error {
		var err error

		bu := tx.Bucket([]byte(table))
		if bu == nil {
			return fmt.Errorf("%s: can't find %s bucket", cn, table)
		}

		rub := bu.Get(d.key(cn))
		if rub == nil {
			return fmt.Errorf("%s: can't find %s", cn, table)
		}

		ub, err := d.decrypt(rub)
		if err != nil {
			return fmt.Errorf("can't decrypt %s info: %s", table, err)
		}

		c, err = decodeCert(cn, ub)
		if err != nil {
			return err
		}

		if table == "server" {
			c.IsServer = true
		}

		return nil
	})

	return c, err
}

// store user cert with the provided password
func (d *database) putcert(c *Cert, pw string, table string) error {
	crt := c.Crt
	sn := crt.Subject.CommonName
	if crt.Raw == nil {
		return fmt.Errorf("%s: Cert is nil?", sn)
	}

	b, err := c.marshal(pw)
	if err != nil {
		return fmt.Errorf("%s: can't marshal cert+key: %s", sn, err)
	}

	eb, err := d.encrypt(b)
	if err != nil {
		return fmt.Errorf("can't encrypt cert info: %s", err)
	}

	es, err := d.encrypt(crt.SerialNumber.Bytes())
	if err != nil {
		return fmt.Errorf("can't encrypt cert serial#: %s", err)
	}

	err = d.db.Update(func(tx *bolt.Tx) error {
		bu := tx.Bucket([]byte(table))
		if bu == nil {
			return fmt.Errorf("%s: can't find %s bucket", sn, table)
		}

		bc := tx.Bucket([]byte("config"))
		if bc == nil {
			return fmt.Errorf("%s: can't find config bucket", sn)
		}

		err := bu.Put(d.key(sn), eb)
		if err != nil {
			return fmt.Errorf("%s: can't write %s info: %s", sn, table, err)
		}

		err = bc.Put(d.key("serial"), es)
		if err != nil {
			return fmt.Errorf("%s: can't write serial#: %s", sn, err)
		}
		return nil
	})

	return err
}

// delete a cert
func (d *database) delcert(cn string, table string) error {
	err := d.db.Update(func(tx *bolt.Tx) error {
		bu := tx.Bucket([]byte(table))
		if bu == nil {
			return fmt.Errorf("%s: can't find %s bucket", cn, table)
		}

		rv := tx.Bucket([]byte("revoked"))
		if bu == nil {
			return fmt.Errorf("%s: can't find revoked bucket", cn)
		}

		k := d.key(cn)
		rub := bu.Get(d.key(cn))
		if rub == nil {
			return fmt.Errorf("%s: can't find %s", cn, table)
		}

		now, err := time.Now().UTC().MarshalBinary()
		if err != nil {
			return fmt.Errorf("%s: can't get time: %s", cn, err)
		}

		et, err := d.encrypt(now)
		if err != nil {
			return fmt.Errorf("%s: can't encrypt time: %s", cn, err)
		}

		// Add the cert on the revoked list. We'll use this to list revoked certs
		// and generate an up-to-date CRL.
		err = rv.Put(et, rub)
		if err != nil {
			return fmt.Errorf("%s: can't add to revoked bucket: %s", cn, err)
		}

		return bu.Delete(k)
	})
	return err
}

// Return server with this config
func (d *database) getsrv(cn string) (*Cert, error) {
	return d.getcert(cn, "server")
}

// Return user with this config
func (d *database) getuser(cn string) (*Cert, error) {
	return d.getcert(cn, "user")
}

// Store server config
func (d *database) putsrv(s *Cert, pw string) error {
	return d.putcert(s, pw, "server")
}

// Store server config
func (d *database) putuser(s *Cert, pw string) error {
	return d.putcert(s, pw, "user")
}

// delete a server config
func (d *database) delsrv(cn string) error {
	return d.delcert(cn, "server")
}

// delete a user config
func (d *database) deluser(cn string) error {
	return d.delcert(cn, "user")
}

// iterators for server block
func (d *database) mapSrv(fp func(s *Cert)) error {
	return d.mapcert("server", fp)
}

// iterators for server block
func (d *database) mapUser(fp func(s *Cert)) error {
	return d.mapcert("user", fp)
}

// iterators for revoked certs
func (d *database) mapRevoked(fp func(t time.Time, c *x509.Certificate)) error {
	err := d.db.View(func(tx *bolt.Tx) error {
		bs := tx.Bucket([]byte("revoked"))
		if bs == nil {
			return fmt.Errorf("can't find revoked bucket")
		}

		err := bs.ForEach(func(k, ev []byte) error {
			tb, err := d.decrypt(k)
			if err != nil {
				return fmt.Errorf("can't decrypt time: %s", err)
			}

			var t time.Time

			err = t.UnmarshalBinary(tb)
			if err != nil {
				return fmt.Errorf("can't decode time: %s", err)
			}

			v, err := d.decrypt(ev)
			if err != nil {
				return fmt.Errorf("can't decrypt revoked cert: %s", err)
			}

			ck, err := decodeCert("$revoked-cert", v)
			if err != nil {
				return err
			}

			fp(t, ck.Crt)
			return nil
		})
		return err
	})
	return err
}

// iterators for user block
func (d *database) mapcert(table string, fp func(c *Cert)) error {
	err := d.db.View(func(tx *bolt.Tx) error {
		bu := tx.Bucket([]byte(table))
		if bu == nil {
			return fmt.Errorf("can't find %s bucket", table)
		}

		err := bu.ForEach(func(k, ev []byte) error {
			v, err := d.decrypt(ev)
			if err != nil {
				return fmt.Errorf("can't decrypt cert info: %s", err)
			}

			c, err := decodeCert("$cert", v)
			if err != nil {
				return err
			}
			if table == "server" {
				c.IsServer = true
			}


			fp(c)
			return nil
		})

		return err
	})

	return err
}

// hash publickey; we use it as a salt for encryption and also SubjectKeyId
func cksum(pk *ecdsa.PublicKey) []byte {
	pm := elliptic.Marshal(pk.Curve, pk.X, pk.Y)
	return hash(pm)
}

func hash(b []byte) []byte {
	h := sha256.New()
	h.Write(b)
	return h.Sum(nil)
}
