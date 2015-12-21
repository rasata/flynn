package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/digitalocean/godo"
	log "github.com/flynn/flynn/Godeps/_workspace/src/gopkg.in/inconshreveable/log15.v2"
	"github.com/flynn/flynn/pkg/azure"
	"github.com/flynn/flynn/pkg/httphelper"
)

var ClusterNotFoundError = errors.New("Cluster not found")

type Installer struct {
	logger log.Logger

	Data *Data
}

func NewInstaller(l log.Logger) *Installer {
	installer := &Installer{
		logger: l,
	}
	data, err := InitData(l)
	if err != nil {
		panic(err)
	}
	installer.Data = data
	return installer
}

func (i *Installer) SubscribeEvents(eventChan chan *Event, lastEventID string) *Subscription {
	return i.Data.SubscribeEvents(eventChan, lastEventID)
}

func (i *Installer) UnsubscribeEvents(sub *Subscription) {
	i.Data.UnsubscribeEvents(sub)
}

func (i *Installer) FindBaseCluster(clusterID string) (*BaseCluster, error) {
	return i.Data.FindBaseCluster(clusterID)
}

func (i *Installer) FindCredentials(credentialID string) (*Credential, error) {
	return i.Data.FindCredentials(credentialID)
}

func (i *Installer) FetchPrompt(clusterID, promptID string) (*Prompt, error) {
	return i.Data.FetchPrompt(clusterID, promptID)
}

func (i *Installer) AzureClient(creds *Credential) *azure.Client {
	return i.Data.AzureClient(creds)
}

var credentialExistsError = errors.New("Credential already exists")

func (i *Installer) SaveCredentials(creds *Credential) error {
	i.Data.dbMtx.Lock()
	defer i.Data.dbMtx.Unlock()
	if _, err := i.FindCredentials(creds.ID); err == nil {
		return credentialExistsError
	}
	tx, err := i.Data.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO credentials (ID, Secret, Name, Type, Endpoint) VALUES ($1, $2, $3, $4, $5);
  `, creds.ID, creds.Secret, creds.Name, creds.Type, creds.Endpoint); err != nil {
		if strings.Contains(err.Error(), "duplicate value") {
			if _, err := tx.Exec(`
				UPDATE credentials SET Secret = $2, Name = $3, Type = $4, Endpoint = $5, DeletedAt = NULL WHERE ID == $1 AND DeletedAt IS NOT NULL
			`, creds.ID, creds.Secret, creds.Name, creds.Type, creds.Endpoint); err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`UPDATE events SET DeletedAt = now() WHERE ResourceType == "credential" AND ResourceID == $1`, creds.ID); err != nil {
				tx.Rollback()
				return err
			}
		} else {
			tx.Rollback()
			return err
		}
	}
	if creds.Type == "azure" {
		for _, oc := range creds.OAuthCreds {
			if _, err := tx.Exec(`
				INSERT INTO oauth_credentials (ClientID, AccessToken, RefreshToken, ExpiresAt, Scope) VALUES ($1, $2, $3, $4, $5);
			`, oc.ClientID, oc.AccessToken, oc.RefreshToken, oc.ExpiresAt, oc.Scope); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	go i.Data.MustSendEvent(&Event{
		Type:         "new_credential",
		ResourceType: "credential",
		ResourceID:   creds.ID,
		Resource:     creds,
	})
	return nil
}

func (i *Installer) DeleteCredentials(id string) error {
	if _, err := i.FindCredentials(id); err != nil {
		return err
	}
	var count int64
	if err := i.Data.db.QueryRow(`SELECT count() FROM clusters WHERE CredentialID == $1 AND DeletedAt IS NULL`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return httphelper.JSONError{
			Code:    httphelper.ConflictErrorCode,
			Message: "Credential is currently being used by one or more clusters",
		}
	}
	if err := i.Data.txExec(`UPDATE credentials SET DeletedAt = now() WHERE ID == $1`, id); err != nil {
		return err
	}
	if err := i.Data.txExec(`UPDATE oauth_credentials SET DeletedAt = now() WHERE ClientID == $1`, id); err != nil {
		return err
	}
	if err := i.Data.txExec(`UPDATE events SET DeletedAt = now() WHERE ResourceType == "credential" AND ResourceID == $1`, id); err != nil {
		return err
	}
	go i.Data.MustSendEvent(&Event{
		Type:         "delete_credential",
		ResourceType: "credential",
		ResourceID:   id,
	})
	return nil
}

func (i *Installer) LaunchCluster(c Cluster) error {
	if err := c.SetDefaultsAndValidate(); err != nil {
		return err
	}

	if err := i.Data.PersistCluster(c); err != nil {
		return err
	}

	base := c.Base()

	i.Data.MustSendEvent(&Event{
		Type:      "new_cluster",
		Cluster:   base,
		ClusterID: base.ID,
	})
	c.Run()
	return nil
}

func (i *Installer) ListDigitalOceanRegions(creds *Credential) (interface{}, error) {
	client := digitalOceanClient(creds)
	regions, r, err := client.Regions.List(&godo.ListOptions{})
	if err != nil {
		code := httphelper.UnknownErrorCode
		if r.StatusCode == 401 {
			code = httphelper.UnauthorizedErrorCode
		}
		return nil, httphelper.JSONError{
			Code:    code,
			Message: err.Error(),
		}
	}
	res := make([]godo.Region, 0, len(regions))
	for _, r := range regions {
		if r.Available {
			res = append(res, r)
		}
	}
	return res, err
}

func (i *Installer) ListAzureRegions(creds *Credential) (interface{}, error) {
	type azureLocation struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	client := i.AzureClient(creds)
	res, err := client.ListLocations("Microsoft.Compute", "virtualMachines")
	if err != nil {
		return nil, err
	}
	locs := make([]azureLocation, 0, len(res))
	for _, l := range res {
		locs = append(locs, azureLocation{
			Name: l,
			Slug: l,
		})
	}
	return locs, nil
}

func (i *Installer) FindCluster(id string) (cluster Cluster, err error) {
	base := &BaseCluster{}
	if err := i.Data.db.QueryRow(`SELECT Type FROM clusters WHERE ID == $1 AND DeletedAt IS NULL`, id).Scan(&base.Type); err != nil {
		return nil, err
	}

	switch base.Type {
	case "aws":
		return i.FindAWSCluster(id)
	case "digital_ocean":
		return i.FindDigitalOceanCluster(id)
	case "azure":
		return i.FindAzureCluster(id)
	case "ssh":
		return i.FindSSHCluster(id)
	default:
		return nil, fmt.Errorf("Invalid cluster type: %s", base.Type)
	}
}

func (i *Installer) FindDigitalOceanCluster(id string) (*DigitalOceanCluster, error) {
	base, err := i.FindBaseCluster(id)
	if err != nil {
		return nil, err
	}

	cluster := &DigitalOceanCluster{
		ClusterID: base.ID,
		base:      base,
	}

	if err := i.Data.db.QueryRow(`SELECT Region, Size, KeyFingerprint FROM digital_ocean_clusters WHERE ClusterID == $1 AND DeletedAt IS NULL LIMIT 1`, base.ID).Scan(&cluster.Region, &cluster.Size, &cluster.KeyFingerprint); err != nil {
		return nil, err
	}

	rows, err := i.Data.db.Query(`SELECT ID FROM digital_ocean_droplets WHERE ClusterID == $1 AND DeletedAt IS NULL`, base.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dropletIDs := make([]int64, 0, base.NumInstances)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		dropletIDs = append(dropletIDs, id)
	}
	cluster.DropletIDs = dropletIDs

	cluster.SetCreds(base.credential)

	return cluster, nil
}

func (i *Installer) FindAzureCluster(id string) (*AzureCluster, error) {
	base, err := i.FindBaseCluster(id)
	if err != nil {
		return nil, err
	}

	cluster := &AzureCluster{
		ClusterID: base.ID,
		base:      base,
	}

	if err := i.Data.db.QueryRow(`SELECT SubscriptionID, Region, Size FROM azure_clusters WHERE ClusterID == $1 AND DeletedAt IS NULL LIMIT 1`, cluster.ClusterID).Scan(&cluster.SubscriptionID, &cluster.Region, &cluster.Size); err != nil {
		return nil, err
	}

	cluster.SetCreds(base.credential)

	return cluster, nil
}

func (i *Installer) FindSSHCluster(id string) (*SSHCluster, error) {
	base, err := i.FindBaseCluster(id)
	if err != nil {
		return nil, err
	}

	cluster := &SSHCluster{
		ClusterID: base.ID,
		base:      base,
	}

	if err := i.Data.db.QueryRow(`SELECT SSHLogin, TargetsJSON FROM ssh_clusters WHERE ClusterID == $1 AND DeletedAt IS NULL LIMIT 1`, cluster.ClusterID).Scan(&cluster.SSHLogin, &cluster.TargetsJSON); err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(cluster.TargetsJSON), &cluster.Targets); err != nil {
		return nil, err
	}

	return cluster, nil
}

func (i *Installer) FindAWSCluster(id string) (*AWSCluster, error) {
	cluster, err := i.FindBaseCluster(id)
	if err != nil {
		return nil, err
	}

	awsCluster := &AWSCluster{
		base: cluster,
	}

	err = i.Data.db.QueryRow(`
	SELECT StackID, StackName, ImageID, Region, InstanceType, VpcCIDR, SubnetCIDR, DNSZoneID FROM aws_clusters WHERE ClusterID == $1 AND DeletedAt IS NULL LIMIT 1
  `, cluster.ID).Scan(&awsCluster.StackID, &awsCluster.StackName, &awsCluster.ImageID, &awsCluster.Region, &awsCluster.InstanceType, &awsCluster.VpcCIDR, &awsCluster.SubnetCIDR, &awsCluster.DNSZoneID)
	if err != nil {
		return nil, err
	}

	awsCreds, err := awsCluster.FindCredentials()
	if err != nil {
		return nil, err
	}
	awsCluster.creds = awsCreds

	return awsCluster, nil
}

func (i *Installer) DeleteCluster(id string) error {
	cluster, err := i.FindCluster(id)
	if err != nil {
		return err
	}
	go cluster.Delete()
	return nil
}
