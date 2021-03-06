/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lib

import (
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	"github.com/cloudflare/cfssl/log"
)

// userHasAttribute returns nil if the user has the attribute, or an
// appropriate error if the user does not have this attribute.
func userHasAttribute(username, attrname string) error {
	val, err := getUserAttrValue(username, attrname)
	if err != nil {
		return err
	}
	if val == "" {
		return fmt.Errorf("user '%s' does not have attribute '%s'", username, attrname)
	}
	return nil
}

// getUserAttrValue returns a user's value for an attribute
func getUserAttrValue(username, attrname string) (string, error) {
	log.Debugf("getUserAttrValue user=%s, attr=%s", username, attrname)
	user, err := UserRegistry.GetUser(username, []string{attrname})
	if err != nil {
		return "", err
	}
	attrval := user.GetAttribute(attrname)
	log.Debugf("getUserAttrValue user=%s, name=%s, value=%s", username, attrname, attrval)
	return attrval, nil

}

// GetCertID returns both the serial number and AKI (Authority Key ID) for the certificate
func GetCertID(bytes []byte) (string, string, error) {
	cert, err := BytesToX509Cert(bytes)
	if err != nil {
		return "", "", err
	}
	serial := cert.SerialNumber.String()
	aki := hex.EncodeToString(cert.AuthorityKeyId)
	return serial, aki, nil
}

// BytesToX509Cert converts bytes (PEM or DER) to an X509 certificate
func BytesToX509Cert(bytes []byte) (*x509.Certificate, error) {
	dcert, _ := pem.Decode(bytes)
	if dcert != nil {
		bytes = dcert.Bytes
	}
	cert, err := x509.ParseCertificate(bytes)
	if err != nil {
		return nil, fmt.Errorf("buffer was neither PEM nor DER encoding: %s", err)
	}
	return cert, err
}
