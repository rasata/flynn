package installer

import (
	"fmt"
	"sync"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/aws"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/gen/cloudformation"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/awslabs/aws-sdk-go/gen/ec2"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/cznic/ql"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/digitalocean/godo"
	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/pkg/azure"
	"github.com/flynn/flynn/pkg/knownhosts"
	"github.com/flynn/flynn/pkg/sshkeygen"
)

type Cluster interface {
	Base() *BaseCluster
	SetDefaultsAndValidate() error
	Run()
	Delete()
	Type() string
	SetBase(*BaseCluster)
	SetCreds(*Credential) error
}

type Credential struct {
	ID         string             `json:"id"`
	Secret     string             `json:"secret"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`     // enum(aws, digital_ocean, azure)
	Endpoint   string             `json:"endpoint"` // token endpoint
	OAuthCreds []*OAuthCredential `json:"oauth_creds,omitempty" ql:"-"`
	DeletedAt  *time.Time         `json:"deleted_at,omitempty"`
}

type OAuthCredential struct {
	ClientID     string     `json:"client_id"` // ClientID == Credential.ID
	AccessToken  string     `json:"access_token"`
	RefreshToken string     `json:"refresh_token"`
	ExpiresAt    *time.Time `json:"expires_at"`
	Scope        string     `json:"scope"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
}

type AWSCluster struct {
	ClusterID    string     `json:"cluster_id" ql:"index xCluster"`
	StackID      string     `json:"stack_id"`
	StackName    string     `json:"stack_name"`
	ImageID      string     `json:"image_id,omitempty"`
	Region       string     `json:"region"`
	InstanceType string     `json:"instance_type"`
	VpcCIDR      string     `json:"vpc_cidr"`
	SubnetCIDR   string     `json:"subnet_cidr"`
	DNSZoneID    string     `json:"dns_zone_id"`
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`

	base  *BaseCluster
	creds aws.CredentialsProvider
	stack *cloudformation.Stack
	cf    *cloudformation.CloudFormation
	ec2   *ec2.EC2
}

type DigitalOceanCluster struct {
	ClusterID      string     `json:"cluster_id" ql:"index xCluster"`
	Region         string     `json:"region"`
	Size           string     `json:"size"`
	KeyFingerprint string     `json:"key_fingerprint"`
	DropletIDs     []int64    `json:"droplet_ids" ql:"-"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`

	base                 *BaseCluster
	client               *godo.Client
	startScript          string
	iptablesConfigScript string
}

type AzureCluster struct {
	ClusterID      string     `json:"cluster_id" ql:"index xCluster"`
	SubscriptionID string     `json:"subscription_id"`
	Region         string     `json:"region"`
	Size           string     `json:"size"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`

	base        *BaseCluster
	client      *azure.Client
	startScript string
}

type DigitalOceanDroplet struct {
	ClusterID string     `json:"cluster_id" ql:"index xCluster"`
	ID        int64      `json:"id"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type TargetServer struct {
	IP        string            `json:"ip"`
	Port      string            `json:"port"`
	User      string            `json:"user"`
	SSHConfig *ssh.ClientConfig `json:"-"`
	SSHClient *ssh.Client       `json:"-"`
}

type SSHCluster struct {
	ClusterID   string          `json:"cluster_id" ql:"index xCluster"`
	DeletedAt   *time.Time      `json:"deleted_at,omitempty"`
	SSHLogin    string          `json:"ssh_login"`
	Targets     []*TargetServer `json:"targets" ql:"-"`
	TargetsJSON string          `json:"-"`

	base       *BaseCluster
	sshAuth    []ssh.AuthMethod
	knownHosts *knownhosts.KnownHosts
}

type BaseCluster struct {
	ID                  string            `json:"id"`
	CredentialID        string            `json:"credential_id"`
	Type                string            `json:"type"`                    // enum(aws, digital_ocean, azure)
	State               string            `json:"state" ql:"index xState"` // enum(starting, error, running, deleting)
	Name                string            `json:"name" ql:"-"`
	NumInstances        int64             `json:"num_instances"`
	ControllerKey       string            `json:"controller_key,omitempty"`
	ControllerPin       string            `json:"controller_pin,omitempty"`
	DashboardLoginToken string            `json:"dashboard_login_token,omitempty"`
	Domain              *Domain           `json:"domain" ql:"-"`
	CACert              string            `json:"ca_cert"`
	SSHKey              *sshkeygen.SSHKey `json:"-" ql:"-"`
	SSHKeyName          string            `json:"ssh_key_name,omitempty"`
	SSHUsername         string            `json:"-" ql:"-"`
	DiscoveryToken      string            `json:"discovery_token"`
	InstanceIPs         []string          `json:"instance_ips,omitempty" ql:"-"`
	DeletedAt           *time.Time        `json:"deleted_at,omitempty"`

	credential        *Credential
	data              *Data
	logger            log.Logger
	done              bool
	passwordPromptMtx sync.Mutex
	passwordCache     map[string]string
}

type InstanceIPs struct {
	ClusterID string `ql:"index xCluster"`
	IP        string
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type Event struct {
	ID           string       `json:"id"`
	Timestamp    time.Time    `json:"timestamp"`
	Type         string       `json:"type"`
	ClusterID    string       `json:"cluster_id,omitempty"`
	Cluster      *BaseCluster `json:"cluster,omitempty" ql:"-"`
	ResourceType string       `json:"resource_type,omitempty"`
	ResourceID   string       `json:"resource_id,omitempty"`
	Resource     interface{}  `json:"resource,omitempty" ql:"-"`
	Description  string       `json:"description,omitempty"`
	DeletedAt    *time.Time   `json:"deleted_at,omitempty"`
}

type Prompt struct {
	ID        string     `json:"id"`
	ClusterID string     `json:"cluster_ID"`
	Type      string     `json:"type,omitempty"`
	Message   string     `json:"message,omitempty"`
	Yes       bool       `json:"yes,omitempty"`
	Input     string     `json:"input,omitempty"`
	Resolved  bool       `json:"resolved,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	resChan   chan *Prompt
	cluster   *BaseCluster
}

func (d *Data) updatedbColumns(in interface{}, t string) error {
	s, err := ql.StructSchema(in)
	if err != nil {
		return err
	}
	rows, err := d.db.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 0", t))
	if err != nil {
		return err
	}
	defer rows.Close()

	var add []string
	var remove []string

	dbColumns, err := rows.Columns()
	if err != nil {
		return err
	}

	fields := make(map[string]ql.Type, len(s.Fields))
	for _, f := range s.Fields {
		fields[f.Name] = f.Type
	}

	dbFieldMap := make(map[string]bool, len(dbColumns))
	for _, c := range dbColumns {
		if _, ok := fields[c]; !ok {
			remove = append(remove, c)
			continue
		}
		dbFieldMap[c] = true
	}

	for c := range fields {
		if _, ok := dbFieldMap[c]; !ok {
			add = append(add, c)
		}
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}

	for _, c := range remove {
		if _, err := tx.Exec(fmt.Sprintf(`
      ALTER TABLE %s DROP COLUMN %s
    `, t, c)); err != nil {
			tx.Rollback()
			return err
		}
	}

	for _, c := range add {
		if _, err := tx.Exec(fmt.Sprintf(`
      ALTER TABLE %s ADD %s %s
    `, t, c, fields[c])); err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// Event PromptID -> ResourceType + ResourceID
func (d *Data) runMigration1() error {
	rows, err := d.db.Query("SELECT * FROM events LIMIT 0")
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return err
	}

	for _, c := range columns {
		if c == "ResourceType" || c == "ResourceID" {
			return nil
		}
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`
    ALTER TABLE events ADD ResourceType string;
    ALTER TABLE events ADD ResourceID string;
    UPDATE events SET ResourceType = "", ResourceID = "";
    DELETE FROM credentials WHERE ID == "aws_env";
  `); err != nil {
		tx.Rollback()
		return err
	}

	for _, c := range columns {
		if c == "PromptID" {
			if _, err := tx.Exec(`UPDATE events SET ResourceType = "prompt", ResourceID = PromptID WHERE PromptID != ""`); err != nil {
				tx.Rollback()
				return err
			}
			break
		}
	}
	return tx.Commit()
}

// Cleanup events for deleted clusters
func (d *Data) runMigration2() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE events SET DeletedAt = now() WHERE ClusterID IN (SELECT ID FROM clusters WHERE DeletedAt IS NOT NULL)
	`); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *Data) MustTableName(item interface{}) string {
	switch item.(type) {
	case *Credential, Credential:
		return "credentials"
	case *OAuthCredential, OAuthCredential:
		return "oauth_credentials"
	case *BaseCluster, BaseCluster:
		return "clusters"
	case *AWSCluster, AWSCluster:
		return "aws_clusters"
	case *DigitalOceanCluster, DigitalOceanCluster:
		return "digital_ocean_clusters"
	case *DigitalOceanDroplet, DigitalOceanDroplet:
		return "digital_ocean_droplets"
	case *AzureCluster, AzureCluster:
		return "azure_clusters"
	case *SSHCluster, SSHCluster:
		return "ssh_clusters"
	case *Event, Event:
		return "events"
	case *Prompt, Prompt:
		return "prompts"
	case *InstanceIPs, InstanceIPs:
		return "instances"
	case *Domain, Domain:
		return "domains"
	}
	panic(fmt.Errorf("Unknown table name for type %T", item))
}

func (d *Data) migrateDB() error {
	typeExamples := []interface{}{
		(*Credential)(nil),
		(*OAuthCredential)(nil),
		(*BaseCluster)(nil),
		(*AWSCluster)(nil),
		(*DigitalOceanCluster)(nil),
		(*DigitalOceanDroplet)(nil),
		(*AzureCluster)(nil),
		(*SSHCluster)(nil),
		(*Event)(nil),
		(*Prompt)(nil),
		(*InstanceIPs)(nil),
		(*Domain)(nil),
	}
	schemaInterfaces := make(map[interface{}]string, len(typeExamples))
	for _, ex := range typeExamples {
		schemaInterfaces[ex] = d.MustTableName(ex)
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	for item, tableName := range schemaInterfaces {
		schema, err := ql.Schema(item, tableName, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(schema.String()); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	if err := d.runMigration1(); err != nil {
		return err
	}

	if err := d.runMigration2(); err != nil {
		return err
	}

	for item, tableName := range schemaInterfaces {
		if err := d.updatedbColumns(item, tableName); err != nil {
			return err
		}
	}

	if err := d.txExec(`
		CREATE UNIQUE INDEX IF NOT EXISTS CredentialsIdx1 ON credentials (ID);
		CREATE INDEX IF NOT EXISTS EventsIdx1 ON events (Type);
		CREATE INDEX IF NOT EXISTS DomainsIdx1 ON domains (ClusterID);
	`); err != nil {
		return err
	}

	return nil
}
