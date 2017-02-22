/*
Copyright IBM Corp. 2017 All Rights Reserved.

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
	"crypto/tls"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/csr"
	"github.com/cloudflare/cfssl/initca"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/universal"
	"github.com/hyperledger/fabric-ca/api"
	"github.com/hyperledger/fabric-ca/lib/dbutil"
	"github.com/hyperledger/fabric-ca/lib/ldap"
	"github.com/hyperledger/fabric-ca/lib/spi"
	"github.com/hyperledger/fabric-ca/util"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/bccsp/factory"
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql" // import to support MySQL
	_ "github.com/lib/pq"              // import to support Postgres
	_ "github.com/mattn/go-sqlite3"    // import to support SQLite3
)

// FIXME: These variables are temporary and will be removed once
// the cobra/viper move is complete and we no longer support the fabric command.
// The correct way is to pass the Server object (and thus ServerConfig)
// to the endpoint handler constructors, thus using no global variables.
var (
	EnrollSigner     signer.Signer
	UserRegistry     spi.UserRegistry
	MaxEnrollments   int
	MyCertDBAccessor *CertDBAccessor
	CAKeyFile        string
	CACertFile       string
	MyCSP            bccsp.BCCSP
)

// Server is the fabric-ca server
type Server struct {
	// The home directory for the server
	HomeDir string
	// BlockingStart if true makes the Start function blocking;
	// It is non-blocking by default.
	BlockingStart bool
	// The server's configuration
	Config *ServerConfig
	// The database handle used to store certificates and optionally
	// the user registry information, unless LDAP it enabled for the
	// user registry function.
	db *sqlx.DB
	// The crypto service provider (BCCSP)
	csp bccsp.BCCSP
	// The certificate DB accessor
	certDBAccessor *CertDBAccessor
	// The user registry
	registry spi.UserRegistry
	// The signer used for enrollment
	enrollSigner signer.Signer
	// The server mux
	mux *http.ServeMux
	// The current listener for this server
	listener net.Listener
	// An error which occurs when serving
	serveError error
}

// Init initializes a fabric-ca server
func (s *Server) Init(renew bool) (err error) {
	// Initialize the config, setting defaults, etc
	err = s.initConfig()
	if err != nil {
		return err
	}

	MyCSP = factory.GetDefault()

	// Initialize key materials
	err = s.initKeyMaterial(renew)
	if err != nil {
		return err
	}
	// Initialize the database
	err = s.initDB()
	if err != nil {
		return err
	}
	// Initialize the enrollment signer
	err = s.initEnrollmentSigner()
	if err != nil {
		return err
	}
	// Successful initialization
	return nil
}

// Start the fabric-ca server
func (s *Server) Start() (err error) {

	s.serveError = nil

	if s.listener != nil {
		return errors.New("server is already started")
	}

	// Initialize the server
	err = s.Init(false)
	if err != nil {
		return err
	}

	// TEMP
	CAKeyFile = s.Config.CA.Keyfile
	CACertFile = s.Config.CA.Certfile

	// Register http handlers
	s.registerHandlers()

	// Start listening and serving
	return s.listenAndServe()

}

// Stop the server
// WARNING: This forcefully closes the listening socket and may cause
// requests in transit to fail, and so is only used for testing.
// A graceful shutdown will be supported with golang 1.8.
func (s *Server) Stop() error {
	if s.listener == nil {
		return errors.New("server is not currently started")
	}
	err := s.listener.Close()
	s.listener = nil
	return err
}

// Initialize the fabric-ca server's key material
func (s *Server) initKeyMaterial(renew bool) error {
	log.Debugf("Init with home %s and config %+v", s.HomeDir, s.Config)

	// Make the path names absolute in the config
	s.makeFileNamesAbsolute()

	keyFile := s.Config.CA.Keyfile
	certFile := s.Config.CA.Certfile

	// If we aren't renewing and the key and cert files exist, do nothing
	if !renew {
		// If they both exist, the server was already initialized
		keyFileExists := util.FileExists(keyFile)
		certFileExists := util.FileExists(certFile)
		if keyFileExists && certFileExists {
			log.Info("The CA key and certificate files already exist")
			log.Infof("Key file location: %s", keyFile)
			log.Infof("Certificate file location: %s", certFile)
			return nil
		}
	}

	// Create the certificate request, copying from config
	ptr := &s.Config.CSR
	req := csr.CertificateRequest{
		CN:    ptr.CN,
		Names: ptr.Names,
		Hosts: ptr.Hosts,
		// FIXME: NewBasicKeyRequest only does ecdsa 256; use config
		KeyRequest:   csr.NewBasicKeyRequest(),
		CA:           ptr.CA,
		SerialNumber: ptr.SerialNumber,
	}

	// Initialize the CA now
	cert, _, key, err := initca.New(&req)
	if err != nil {
		return fmt.Errorf("Failed to initialize CA [%s]\nRequest was %#v", err, req)
	}

	// Store the key and certificate to file
	err = writeFile(keyFile, key, 0600)
	if err != nil {
		return fmt.Errorf("Failed to store key: %s", err)
	}
	err = writeFile(certFile, cert, 0644)
	if err != nil {
		return fmt.Errorf("Failed to store certificate: %s", err)
	}
	log.Info("The CA key and certificate files were generated")
	log.Infof("Key file location: %s", keyFile)
	log.Infof("Certificate file location: %s", certFile)
	return nil
}

// RegisterBootstrapUser registers the bootstrap user with appropriate privileges
func (s *Server) RegisterBootstrapUser(user, pass, affiliation string) error {
	// Initialize the config, setting defaults, etc
	if user == "" || pass == "" {
		return errors.New("empty user and/or pass not allowed")
	}
	err := s.initConfig()
	if err != nil {
		return fmt.Errorf("Failed to register bootstrap user '%s': %s", user, err)
	}
	id := ServerConfigIdentity{
		ID:          user,
		Pass:        pass,
		Type:        "user",
		Affiliation: affiliation,
		Attributes: map[string]string{
			"hf.Registrar.Roles":         "client,user,peer,validator,auditor",
			"hf.Registrar.DelegateRoles": "client,user,validator,auditor",
			"hf.Revoker":                 "true",
		},
	}
	registry := &s.Config.Registry
	registry.Identities = append(registry.Identities, id)
	log.Debugf("Registered bootstrap identity: %+v", id)
	return nil
}

// Do any ize the config, setting any defaults and making filenames absolute
func (s *Server) initConfig() (err error) {
	// Init home directory if not set
	if s.HomeDir == "" {
		s.HomeDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("Failed to initialize server's home directory: %s", err)
		}
	}
	// Init config if not set
	if s.Config == nil {
		s.Config = new(ServerConfig)
	}
	// Set config defaults
	cfg := s.Config
	if cfg.Address == "" {
		cfg.Address = DefaultServerAddr
	}
	if cfg.Port == 0 {
		cfg.Port = DefaultServerPort
	}
	if cfg.CA.Certfile == "" {
		cfg.CA.Certfile = "ca-cert.pem"
	}
	if cfg.CA.Keyfile == "" {
		cfg.CA.Keyfile = "ca-key.pem"
	}
	if cfg.CSR.CN == "" {
		cfg.CSR.CN = "fabric-ca-server"
	}
	// Set log level if debug is true
	if cfg.Debug {
		log.Level = log.LevelDebug
	}
	// Init the BCCSP
	err = factory.InitFactories(s.Config.CSP)
	if err != nil {
		panic(fmt.Errorf("Could not initialize BCCSP Factories [%s]", err))
	}

	return nil
}

// Initialize the database for the server
func (s *Server) initDB() error {
	db := &s.Config.DB

	log.Debugf("Initializing '%s' data base at '%s'", db.Type, db.Datasource)

	var err error
	var exists bool

	if db.Type == "" {
		db.Type = "sqlite3"
	}
	if db.Datasource == "" {
		var ds string
		ds, err = util.MakeFileAbs("fabric-ca-server.db", s.HomeDir)
		if err != nil {
			return err
		}
		db.Datasource = ds
	}
	switch db.Type {
	case "sqlite3":
		s.db, exists, err = dbutil.NewUserRegistrySQLLite3(db.Datasource)
		if err != nil {
			return err
		}
	case "postgres":
		s.db, exists, err = dbutil.NewUserRegistryPostgres(db.Datasource, &db.TLS)
		if err != nil {
			return err
		}
	case "mysql":
		s.db, exists, err = dbutil.NewUserRegistryMySQL(db.Datasource, &db.TLS)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("Invalid db.type in config file: '%s'; must be 'sqlite3', 'postgres', or 'mysql'", db.Type)
	}

	// Set the certificate DB accessor
	s.certDBAccessor = NewCertDBAccessor(s.db)
	MyCertDBAccessor = s.certDBAccessor

	// Initialize the user registry.
	// If LDAP is not configured, the fabric-ca server functions as a user
	// registry based on the database.
	err = s.initUserRegistry()
	if err != nil {
		return err
	}

	// If the DB doesn't exist, bootstrap it
	if !exists {
		err = s.loadUsersTable()
		if err != nil {
			return err
		}
		err = s.loadAffiliationsTable()
		if err != nil {
			return err
		}
	}
	log.Infof("Initialized %s data base at %s", db.Type, db.Datasource)
	return nil
}

// Initialize the user registry interface
func (s *Server) initUserRegistry() error {
	log.Debug("Initializing user registry")
	var err error
	ldapCfg := &s.Config.LDAP

	if ldapCfg.Enabled {
		// Use LDAP for the user registry
		s.registry, err = ldap.NewClient(ldapCfg)
		UserRegistry = s.registry
		log.Debugf("Initialized LDAP user registry; err=%s", err)
		return err
	}

	// Use the DB for the user registry
	dbAccessor := new(Accessor)
	dbAccessor.SetDB(s.db)
	s.registry = dbAccessor
	UserRegistry = s.registry
	log.Debug("Initialized DB user registry")
	return nil
}

// Initialize the enrollment signer
func (s *Server) initEnrollmentSigner() (err error) {

	c := s.Config

	// If there is a config, use its signing policy. Otherwise create a default policy.
	var policy *config.Signing
	if c.Signing != nil {
		policy = c.Signing
	} else {
		policy = &config.Signing{
			Profiles: map[string]*config.SigningProfile{},
			Default:  config.DefaultConfig(),
		}
	}

	// Make sure the policy reflects the new remote
	if c.Remote != "" {
		err = policy.OverrideRemotes(c.Remote)
		if err != nil {
			return fmt.Errorf("Failed initializing enrollment signer: %s", err)
		}
	}

	// Get CFSSL's universal root and signer
	root := universal.Root{
		Config: map[string]string{
			"cert-file": c.CA.Certfile,
			"key-file":  c.CA.Keyfile,
		},
		ForceRemote: c.Remote != "",
	}
	s.enrollSigner, err = universal.NewSigner(root, policy)
	EnrollSigner = s.enrollSigner
	if err != nil {
		return err
	}
	s.enrollSigner.SetDBAccessor(s.certDBAccessor)

	// Successful enrollment
	return nil
}

// Register all endpoint handlers
func (s *Server) registerHandlers() {
	s.mux = http.NewServeMux()
	s.registerHandlerLog("register", NewRegisterHandler)
	s.registerHandlerLog("enroll", NewEnrollHandler)
	s.registerHandlerLog("reenroll", NewReenrollHandler)
	s.registerHandlerLog("revoke", NewRevokeHandler)
	s.registerHandlerLog("tcert", NewTCertHandler)
}

// Register an endpoint handler and log success or error
func (s *Server) registerHandlerLog(
	path string,
	getHandler func() (http.Handler, error)) {
	err := s.registerHandler(path, getHandler)
	if err != nil {
		log.Warningf("Endpoint '%s' is disabled: %s", path, err)
	} else {
		log.Infof("Endpoint '%s' is enabled", path)
	}
}

// Register an endpoint handler and return an error if unsuccessful
func (s *Server) registerHandler(
	path string,
	getHandler func() (http.Handler, error)) (err error) {

	var handler http.Handler

	handler, err = getHandler()
	if err != nil {
		return fmt.Errorf("Endpoint '%s' is disabled: %s", path, err)
	}
	path, handler, err = NewAuthWrapper(path, handler, err)
	if err != nil {
		return fmt.Errorf("Endpoint '%s' has been disabled: %s", path, err)
	}
	s.mux.Handle(path, handler)
	return nil
}

// Starting listening and serving
func (s *Server) listenAndServe() (err error) {

	var listener net.Listener

	c := s.Config

	// Set default listening address and port
	if c.Address == "" {
		c.Address = DefaultServerAddr
	}
	if c.Port == 0 {
		c.Port = DefaultServerPort
	}
	addr := net.JoinHostPort(c.Address, strconv.Itoa(c.Port))

	if c.TLS.Enabled {
		log.Debug("TLS is enabled")
		var cer tls.Certificate
		cer, err = tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
		if err != nil {
			return err
		}
		config := &tls.Config{Certificates: []tls.Certificate{cer}}
		listener, err = tls.Listen("tcp", addr, config)
		if err != nil {
			return fmt.Errorf("TLS listen failed: %s", err)
		}
		log.Infof("Listening at https://%s", addr)
	} else {
		listener, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("TCP listen failed: %s", err)
		}
		log.Infof("Listening at http://%s", addr)
	}
	s.listener = listener

	// Start serving requests, either blocking or non-blocking
	if s.BlockingStart {
		return s.serve()
	}
	go s.serve()
	return nil
}

func (s *Server) serve() error {
	s.serveError = http.Serve(s.listener, s.mux)
	log.Errorf("Server has stopped serving: %s", s.serveError)
	if s.listener != nil {
		s.listener.Close()
		s.listener = nil
	}
	return s.serveError
}

// loadUsersTable adds the configured users to the table if not already found
func (s *Server) loadUsersTable() error {
	log.Debug("Loading users table")
	registry := &s.Config.Registry
	for _, id := range registry.Identities {
		log.Debugf("Loading identity '%s'", id.ID)
		err := s.addIdentity(&id, false)
		if err != nil {
			return err
		}
	}
	log.Debug("Successfully loaded users table")
	return nil
}

// loadAffiliationsTable adds the configured affiliations to the table
func (s *Server) loadAffiliationsTable() error {
	log.Debug("Loading affiliations table")
	err := s.loadAffiliationsTableR(s.Config.Affiliations, "")
	if err == nil {
		log.Debug("Successfully loaded affiliations table")
	}
	return err
}

// Recursive function to load the affiliations table hierarchy
func (s *Server) loadAffiliationsTableR(val interface{}, parentPath string) (err error) {
	var path string
	if val == nil {
		return nil
	}
	switch val.(type) {
	case string:
		path = affiliationPath(val.(string), parentPath)
		err = s.addAffiliation(path, parentPath)
		if err != nil {
			return err
		}
	case []string:
		for _, ele := range val.([]string) {
			err = s.loadAffiliationsTableR(ele, parentPath)
			if err != nil {
				return err
			}
		}
	case []interface{}:
		for _, ele := range val.([]interface{}) {
			err = s.loadAffiliationsTableR(ele, parentPath)
			if err != nil {
				return err
			}
		}
	default:
		for name, ele := range val.(map[string]interface{}) {
			path = affiliationPath(name, parentPath)
			err = s.addAffiliation(path, parentPath)
			if err != nil {
				return err
			}
			err = s.loadAffiliationsTableR(ele, path)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Add an identity to the registry
func (s *Server) addIdentity(id *ServerConfigIdentity, errIfFound bool) error {
	user, _ := s.registry.GetUser(id.ID, nil)
	if user != nil {
		if errIfFound {
			return fmt.Errorf("Identity '%s' is already registered", id.ID)
		}
		log.Debugf("Loaded identity: %+v", id)
		return nil
	}
	maxEnrollments, err := s.getMaxEnrollments(id.MaxEnrollments)
	if err != nil {
		return err
	}
	rec := spi.UserInfo{
		Name:           id.ID,
		Pass:           id.Pass,
		Type:           id.Type,
		Group:          id.Affiliation,
		Attributes:     s.convertAttrs(id.Attributes),
		MaxEnrollments: maxEnrollments,
	}
	err = s.registry.InsertUser(rec)
	if err != nil {
		return fmt.Errorf("Failed to insert user '%s': %s", id.ID, err)
	}
	log.Debugf("Registered identity: %+v", id)
	return nil
}

func (s *Server) addAffiliation(path, parentPath string) error {
	log.Debugf("Adding affiliation %s", path)
	return s.registry.InsertGroup(path, parentPath)
}

func (s *Server) convertAttrs(inAttrs map[string]string) []api.Attribute {
	outAttrs := make([]api.Attribute, 0)
	for name, value := range inAttrs {
		outAttrs = append(outAttrs, api.Attribute{
			Name:  name,
			Value: value,
		})
	}
	return outAttrs
}

// Get max enrollments relative to the configured max
func (s *Server) getMaxEnrollments(requestedMax int) (int, error) {
	configuredMax := s.Config.Registry.MaxEnrollments
	if requestedMax < 0 {
		return configuredMax, nil
	}
	if configuredMax == 0 {
		// no limit, so grant any request
		return requestedMax, nil
	}
	if requestedMax == 0 && configuredMax != 0 {
		return 0, fmt.Errorf("Infinite enrollments is not permitted; max is %d",
			configuredMax)
	}
	if requestedMax > configuredMax {
		return 0, fmt.Errorf("Max enrollments of %d is not permitted; max is %d",
			requestedMax, configuredMax)
	}
	return requestedMax, nil
}

// Make all file names in the config absolute
func (s *Server) makeFileNamesAbsolute() error {
	fields := []*string{
		&s.Config.CA.Certfile,
		&s.Config.CA.Keyfile,
		&s.Config.TLS.CertFile,
		&s.Config.TLS.KeyFile,
	}
	for _, namePtr := range fields {
		abs, err := util.MakeFileAbs(*namePtr, s.HomeDir)
		if err != nil {
			return err
		}
		*namePtr = abs
	}
	return nil
}

func writeFile(file string, buf []byte, perm os.FileMode) error {
	err := os.MkdirAll(filepath.Dir(file), perm)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(file, buf, perm)
}

func affiliationPath(name, parent string) string {
	if parent == "" {
		return name
	}
	return fmt.Sprintf("%s.%s", parent, name)
}
