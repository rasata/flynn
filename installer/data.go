package installer

import (
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/cznic/ql"
	_ "github.com/flynn/flynn/Godeps/_workspace/src/github.com/cznic/ql/driver" // ql driver for database/sql
	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/cli/config"
	"github.com/flynn/flynn/pkg/sshkeygen"
)

var keysDir, dbPath string

func init() {
	dir := filepath.Join(config.Dir(), "installer")
	keysDir = filepath.Join(dir, "keys")
	dbPath = filepath.Join(dir, "data.db")
}

func saveSSHKey(name string, key *sshkeygen.SSHKey) error {
	if err := os.MkdirAll(keysDir, 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(keysDir, name), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := pem.Encode(f, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key.PrivateKey)}); err != nil {
		return err
	}

	if err := ioutil.WriteFile(filepath.Join(keysDir, fmt.Sprintf("%s.pub", name)), key.PublicKey, 0644); err != nil {
		return err
	}
	return nil
}

func loadSSHKey(name string) (*sshkeygen.SSHKey, error) {
	key := &sshkeygen.SSHKey{}
	data, err := ioutil.ReadFile(filepath.Join(keysDir, name))
	if err != nil {
		return nil, err
	}
	b, _ := pem.Decode(data)
	key.PrivateKey, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}

	key.PublicKey, err = ioutil.ReadFile(filepath.Join(keysDir, name+".pub"))
	if err != nil {
		return nil, err
	}
	return key, nil
}

type Data struct {
	Clusters    []Cluster     `json:"clusters"`
	Credentials []*Credential `json:"credentials"`

	subscriptions    []*Subscription
	subscriptionsMtx sync.Mutex

	prompts    map[string]*Prompt
	promptsMtx sync.Mutex

	clusters    []Cluster
	clustersMtx sync.RWMutex

	db    *sql.DB
	dbMtx sync.RWMutex

	logger log.Logger
}

func InitData(l log.Logger) (*Data, error) {
	d := &Data{logger: l}

	db, err := d.openDB()
	if err != nil {
		return nil, err
	}
	d.db = db
	if err := d.migrateDB(); err != nil {
		return nil, err
	}

	clusters, err := d.FetchClusters()
	if err != nil {
		return nil, err
	}
	d.Clusters = clusters

	creds, err := d.FetchCredentials()
	if err != nil {
		return nil, err
	}
	d.Credentials = creds

	return d, nil
}

func (d *Data) openDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("ql", dbPath)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return db, nil
}

func (d *Data) txExec(query string, args ...interface{}) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec(query, args...)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (d *Data) ScanItem(item reflect.Value, cols []string, scanFn func(...interface{}) error) error {
	fields := make([]interface{}, len(cols))
	for idx, col := range cols {
		fields[idx] = item.FieldByName(col).Pointer()
	}
	if err := scanFn(fields...); err != nil {
		return err
	}
	return nil
}

func (d *Data) FetchItems(example interface{}) (interface{}, error) {
	tableName := d.MustTableName(example)
	cols, err := d.Columns(tableName)
	if err != nil {
		return nil, err
	}
	typ := reflect.TypeOf(example)
	items := reflect.MakeSlice(typ, 0, 0)
	rows, err := d.db.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE DeletedAt IS NULL`, strings.Join(cols, ", "), tableName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item := reflect.New(typ)
		if err := d.ScanItem(item, cols, rows.Scan); err != nil {
			return nil, err
		}
		items = reflect.AppendSlice(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items.Interface(), nil
}

func (d *Data) FetchItem(example interface{}, conditions string, args ...interface{}) (interface{}, error) {
	tableName := d.MustTableName(example)
	cols, err := d.Columns(tableName)
	if err != nil {
		return nil, err
	}
	typ := reflect.TypeOf(example)
	item := reflect.New(typ)
	query := fmt.Sprintf("SELECT %s FROM %s WHERE DeletedAt IS NULL", strings.Join(cols, ", "), tableName)
	if conditions != "" {
		query += " AND "
		query += conditions
	}
	if err := d.ScanItem(item, cols, d.db.QueryRow(query, args...).Scan); err != nil {
		return nil, err
	}
	return item, nil
}

func (d *Data) FetchClusters() ([]Cluster, error) {
	items, err := d.FetchItems(&BaseCluster{})
	if err != nil {
		return nil, err
	}
	baseClusters, ok := items.([]*BaseCluster)
	if !ok {
		return nil, fmt.Errorf("Invalid base clusters slice %T", items)
	}

	clusters := make([]Cluster, 0, len(baseClusters))
	for _, base := range baseClusters {
		var example interface{}
		switch base.Type {
		case "aws":
			example = &AWSCluster{}
		case "digital_ocean":
			example = &DigitalOceanCluster{}
		case "azure":
			example = &AzureCluster{}
		case "ssh":
			example = &SSHCluster{}
		default:
			panic(fmt.Errorf("Unknown cluster type %s", base.Type))
		}

		item, err := d.FetchItem(example, "ClusterID = $1", []interface{}{base.ID})
		if err != nil {
			return nil, err
		}
		cluster, ok := item.(Cluster)
		if !ok {
			return nil, fmt.Errorf("Expected item to be a Cluster, got %T", item)
		}
		cluster.SetBase(base)
		clusters = append(clusters, cluster)
	}
	return clusters, nil
}

func (d *Data) FetchCredentials() ([]*Credential, error) {
	creds := []*Credential{}
	rows, err := d.db.Query(`SELECT ID, Secret, Name, Type, Endpoint FROM credentials WHERE DeletedAt IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint *string
		c := &Credential{}
		if err := rows.Scan(&c.ID, &c.Secret, &c.Name, &c.Type, &endpoint); err != nil {
			return nil, err
		}
		if c.Type == "azure" {
			oauthCreds, err := d.FetchOAuthCredentials(c.ID)
			if err != nil {
				return nil, err
			}
			c.OAuthCreds = oauthCreds
		}
		creds = append(creds, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return creds, nil
}

func (d *Data) FetchOAuthCredentials(clientID string) ([]*OAuthCredential, error) {
	oauthCreds := make([]*OAuthCredential, 0, 2)
	rows, err := d.db.Query(`SELECT AccessToken, RefreshToken, ExpiresAt, Scope FROM oauth_credentials WHERE ClientID == $1`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		oc := &OAuthCredential{ClientID: clientID}
		if err := rows.Scan(&oc.AccessToken, &oc.RefreshToken, &oc.ExpiresAt, &oc.Scope); err != nil {
			return nil, err
		}
		oauthCreds = append(oauthCreds, oc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return oauthCreds, nil
}

func (d *Data) FetchEventsSince(lastID string) ([]*Event, error) {
	var ts time.Time
	if lastID != "" {
		nano, err := strconv.ParseInt(strings.TrimPrefix(lastID, "event-"), 10, 64)
		if err != nil {
			return nil, err
		} else {
			ts = time.Unix(0, nano)
		}
	}

	var events []*Event
	rows, err := d.db.Query(`
    SELECT ID, Timestamp, Type, ClusterID, ResourceType, ResourceID, Description FROM events WHERE DeletedAt IS NULL AND Timestamp > $1 ORDER BY Timestamp
  `, ts)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		e := &Event{}
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Type, &e.ClusterID, &e.ResourceType, &e.ResourceID, &e.Description); err != nil {
			return nil, err
		}
		if err := d.processEvent(e); err != nil {
			// TODO(jvatic): Log error
			continue
		}
		events = append(events, e)
	}
	return events, nil
}

func (d *Data) findCachedCluster(clusterID string) (Cluster, error) {
	d.clustersMtx.RLock()
	defer d.clustersMtx.RUnlock()
	for _, c := range d.clusters {
		if c.Base().ID == clusterID {
			return c, nil
		}
	}
	return nil, fmt.Errorf("No cached cluster with id %q found", clusterID)
}

func (d *Data) FindBaseCluster(clusterID string) (*BaseCluster, error) {
	if cluster, err := d.findCachedCluster(clusterID); err == nil {
		return cluster.Base(), nil
	}

	c := &BaseCluster{ID: clusterID, data: d, logger: d.logger}

	err := d.db.QueryRow(`
	SELECT CredentialID, Type, State, NumInstances, ControllerKey, ControllerPin, DashboardLoginToken, CACert, SSHKeyName, DiscoveryToken FROM clusters WHERE ID == $1 AND DeletedAt IS NULL LIMIT 1
  `, c.ID).Scan(&c.CredentialID, &c.Type, &c.State, &c.NumInstances, &c.ControllerKey, &c.ControllerPin, &c.DashboardLoginToken, &c.CACert, &c.SSHKeyName, &c.DiscoveryToken)
	if err != nil {
		return nil, err
	}

	domain := &Domain{ClusterID: c.ID}
	err = d.db.QueryRow(`
  SELECT Name, Token FROM domains WHERE ClusterID == $1 AND DeletedAt IS NULL LIMIT 1
  `, c.ID).Scan(&domain.Name, &domain.Token)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		c.Domain = domain
	}

	var instanceIPs []string
	rows, err := d.db.Query(`SELECT IP FROM instances WHERE ClusterID == $1 AND DeletedAt IS NULL`, c.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ip string
		err = rows.Scan(&ip)
		if err != nil {
			return nil, err
		}
	}
	c.InstanceIPs = instanceIPs

	if c.Type != "ssh" {
		credential, err := d.FindCredentials(c.CredentialID)
		if err != nil {
			return nil, err
		}
		c.credential = credential
	}

	return c, nil
}

func (d *Data) FindCredentials(credentialID string) (*Credential, error) {
	creds := &Credential{}
	var endpoint *string
	if err := d.db.QueryRow(`SELECT ID, Secret, Name, Type, Endpoint FROM credentials WHERE ID == $1 AND DeletedAt IS NULL LIMIT 1`, credentialID).Scan(&creds.ID, &creds.Secret, &creds.Name, &creds.Type, &endpoint); err != nil {
		return nil, err
	}
	if endpoint != nil {
		creds.Endpoint = *endpoint
	}
	if creds.Type == "azure" {
		oauthCreds, err := d.FetchOAuthCredentials(creds.ID)
		if err != nil {
			return nil, err
		}
		creds.OAuthCreds = oauthCreds
	}
	return creds, nil
}

func (d *Data) processEvent(event *Event) error {
	var err error
	var cluster *BaseCluster
	if event.ClusterID != "" {
		cluster, err = d.FindBaseCluster(event.ClusterID) // TODO(jvatic): implement d.FindBaseCluster
		if err != nil {
			return fmt.Errorf("Error finding cluster %s", event.ClusterID)
		}
		if event.Type == "log" && cluster.State == "running" {
			return fmt.Errorf("Log events can't be sent for running cluster (%s)", cluster.ID)
		}
	}
	switch event.ResourceType {
	case "":
	case "prompt":
		p := &Prompt{}
		if err := d.db.QueryRow(`SELECT ID, Type, Message, Yes, Input, Resolved FROM prompts WHERE ID == $1 AND DeletedAt IS NULL`, event.ResourceID).Scan(&p.ID, &p.Type, &p.Message, &p.Yes, &p.Input, &p.Resolved); err != nil {
			return fmt.Errorf("GetEventsSince Prompt Scan Error: %s", err.Error())
		}
		event.Resource = p
	case "credential":
		if event.Type == "new_credential" {
			creds := &Credential{}
			if err := d.db.QueryRow(`SELECT Type, Name, ID FROM credentials WHERE ID == $1 AND DeletedAt IS NULL`, event.ResourceID).Scan(&creds.Type, &creds.Name, &creds.ID); err != nil {
				if err != sql.ErrNoRows {
					return fmt.Errorf("Credential Scan Error: %s", err.Error())
				}
				return fmt.Errorf("Credential not found with id %s: %s", event.ResourceID, err.Error())
			}
			event.Resource = creds
		}
	default:
		return fmt.Errorf("Unknown event.ResourceType: %q", event.ResourceType)
	}
	return nil
}

func (d *Data) Columns(tableName string) ([]string, error) {
	rows, err := d.db.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 0", tableName))
	if err != nil {
		return nil, err
	}
	return rows.Columns()
}

func (d *Data) MarshalItem(tableName string, item interface{}) ([]interface{}, error) {
	cols, err := d.Columns(tableName)
	if err != nil {
		return nil, err
	}

	v := reflect.Indirect(reflect.ValueOf(item))
	fields := make([]interface{}, len(cols))
	for idx, c := range cols {
		fields[idx] = v.FieldByName(c).Interface()
	}
	return fields, nil
}

func (d *Data) PersistItemWithTX(item interface{}, tx *sql.Tx) error {
	d.dbMtx.Lock()
	defer d.dbMtx.Unlock()

	tableName := d.MustTableName(item)

	fields, err := d.MarshalItem(tableName, item)
	if err != nil {
		return err
	}

	if prompt, ok := item.(*Prompt); ok {
		d.promptsMtx.Lock()
		d.prompts[prompt.ClusterID] = prompt
		d.promptsMtx.Unlock()
	}

	vStr := make([]string, 0, len(fields))
	for idx := range fields {
		vStr = append(vStr, fmt.Sprintf("$%d", idx+1))
	}
	list, err := ql.Compile(fmt.Sprintf(`
    INSERT INTO %s VALUES(%s);
	`, tableName, strings.Join(vStr, ", ")))
	if err != nil {
		return err
	}
	_, err = tx.Exec(list.String(), fields...)
	if err != nil {
		tx.Rollback()
		return err
	}
	return nil
}

func (d *Data) PersistItem(item interface{}) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	if err := d.PersistItemWithTX(item, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *Data) PersistCluster(c Cluster) error {
	d.dbMtx.Lock()
	defer d.dbMtx.Unlock()

	base := c.Base()

	base.Type = c.Type()
	base.Name = base.ID

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	if err := d.PersistItemWithTX(base, tx); err != nil {
		return err
	}
	if err := d.PersistItemWithTX(c, tx); err != nil {
		return err
	}

	d.clustersMtx.Lock()
	cached := false
	for _, cluster := range d.clusters {
		if cluster.Base().ID == base.ID {
			cached = true
			break
		}
	}
	if !cached {
		d.clusters = append(d.clusters, c)
	}
	d.clustersMtx.Unlock()

	return tx.Commit()
}

func (d *Data) DeleteCluster(clusterID string) error {
	d.clustersMtx.Lock()
	defer d.clustersMtx.Unlock()
	clusters := make([]Cluster, 0, len(d.clusters))
	for _, c := range d.clusters {
		if c.Base().ID != clusterID {
			clusters = append(clusters, c)
		}
	}
	d.clusters = clusters

	var tx *sql.Tx
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}

	if _, err = tx.Exec(`UPDATE prompts SET DeletedAt = now() WHERE ID IN (SELECT ResourclusterID FROM events WHERE ClusterID == $1 AND ResourceType == "prompt")`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE events SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE domains SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE instances SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE clusters SET DeletedAt = now() WHERE ID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE aws_clusters SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE digital_ocean_clusters SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE digital_ocean_droplets SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	if _, err = tx.Exec(`UPDATE ssh_clusters SET DeletedAt = now() WHERE ClusterID == $1`, clusterID); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

func (d *Data) FetchPrompt(clusterID, promptID string) (*Prompt, error) {
	d.promptsMtx.Lock()
	defer d.promptsMtx.Unlock()
	if prompt, ok := d.prompts[clusterID]; ok && prompt.ID == promptID {
		return prompt, nil
	}
	return nil, fmt.Errorf("FetchPrompt error: No active prompt found for cluster %s matching id %s", clusterID, promptID)
}

func (d *Data) UpdatePrompt(prompt *Prompt) error {
	d.dbMtx.Lock()
	defer d.dbMtx.Unlock()
	return d.txExec(`UPDATE prompts SET Resolved = $1, Yes = $2, Input = $3 WHERE ID == $4`, prompt.Resolved, prompt.Yes, prompt.Input, prompt.ID)
}
