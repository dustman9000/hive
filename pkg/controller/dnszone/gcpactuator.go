package dnszone

import (
	"net/http"
	"strings"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1"
	"github.com/openshift/hive/pkg/gcpclient"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	googleapi "google.golang.org/api/googleapi"

	dns "google.golang.org/api/dns/v1"
	corev1 "k8s.io/api/core/v1"

	controllerutils "github.com/openshift/hive/pkg/controller/utils"
)

const (
	zoneNotEmptyReason = "containerNotEmpty"
)

// GCPActuator attempts to make the current state reflect the given desired state.
type GCPActuator struct {
	// logger is the logger used for this controller
	logger log.FieldLogger

	// gcpClient is a utility for making it easy for controllers to interface with GCP
	gcpClient gcpclient.Client

	// dnsZone is the DNSZone that represents the desired state.
	dnsZone *hivev1.DNSZone

	// managedZone is the GCP Cloud DNS Managed zone object.
	managedZone *dns.ManagedZone
}

const managedByHiveDescription = "Managed by Hive."

type gcpClientBuilderType func(secret *corev1.Secret) (gcpclient.Client, error)

// NewGCPActuator creates a new GCPActuator object. A new GCPActuator is expected to be created for each controller sync.
func NewGCPActuator(
	logger log.FieldLogger,
	secret *corev1.Secret,
	dnsZone *hivev1.DNSZone,
	gcpClientBuilder gcpClientBuilderType,
) (*GCPActuator, error) {
	gcpClient, err := gcpClientBuilder(secret)
	if err != nil {
		logger.WithError(err).Error("Error creating GCPClient")
		return nil, err
	}

	gcpActuator := &GCPActuator{
		logger:    logger,
		gcpClient: gcpClient,
		dnsZone:   dnsZone,
	}

	return gcpActuator, nil
}

// Ensure GCPActuator implements the Actuator interface. This will fail at compile time when false.
var _ Actuator = &GCPActuator{}

// Create implements the Create call of the actuator interface
func (a *GCPActuator) Create() error {
	logger := a.logger.WithField("zone", a.dnsZone.Spec.Zone)
	logger.Info("Creating managed zone")

	zone := a.dnsZone.Spec.Zone
	managedZone, err := a.gcpClient.CreateManagedZone(
		&dns.ManagedZone{
			Name:        generateManagedZoneName(zone),
			Description: managedByHiveDescription,
			DnsName:     controllerutils.Dotted(zone),
		},
	)

	if err != nil {
		logger.WithError(err).Error("Error creating managed zone")
		return err
	}

	logger.Debug("Managed zone successfully created")
	a.managedZone = managedZone
	return nil
}

// Delete implements the Delete call of the actuator interface
func (a *GCPActuator) Delete() error {
	if a.managedZone == nil {
		return errors.New("managedZone is unpopulated")
	}

	logger := a.logger.WithField("zone", a.dnsZone.Spec.Zone).WithField("zoneName", a.managedZone.Name)

	if err := a.deleteRecordSets(logger); err != nil {
		return err
	}

	logger.Info("Deleting managed zone")
	err := a.gcpClient.DeleteManagedZone(a.managedZone.Name)
	if err != nil {
		logLevel := log.ErrorLevel
		if gcpErr, ok := err.(*googleapi.Error); ok && gcpErr.Code == http.StatusBadRequest {
			for _, e := range gcpErr.Errors {
				if e.Reason == zoneNotEmptyReason {
					logLevel = log.InfoLevel
					break
				}
			}
		}
		log.WithError(err).Log(logLevel, "Cannot delete managed zone")
	}
	return err
}

func (a *GCPActuator) deleteRecordSets(logger log.FieldLogger) error {
	logger.Info("Deleting recordsets in managedzone")
	listOpts := gcpclient.ListResourceRecordSetsOptions{}
	for {
		listOutput, err := a.gcpClient.ListResourceRecordSets(a.managedZone.Name, listOpts)
		if err != nil {
			return err
		}
		var recordSetsToDelete []*dns.ResourceRecordSet
		for _, recordSet := range listOutput.Rrsets {
			// Ignore the 2 recordsets that are created with the managed zone and that cannot be deleted
			if n, t := recordSet.Name, recordSet.Type; n == controllerutils.Dotted(a.dnsZone.Spec.Zone) && (t == "NS" || t == "SOA") {
				continue
			}
			logger.WithField("name", recordSet.Name).WithField("type", recordSet.Type).Info("recordset set for deletion")
			recordSetsToDelete = append(recordSetsToDelete, recordSet)
		}
		if len(recordSetsToDelete) > 0 {
			logger.WithField("count", len(recordSetsToDelete)).Info("deleting recordsets")
			if err := a.gcpClient.DeleteResourceRecordSets(a.managedZone.Name, recordSetsToDelete); err != nil {
				return err
			}
		}
		if listOutput.NextPageToken == "" {
			break
		}
		listOpts.PageToken = listOutput.NextPageToken
	}
	return nil
}

// Exists implements the Exists call of the actuator interface
func (a *GCPActuator) Exists() (bool, error) {
	return a.managedZone != nil, nil
}

// UpdateMetadata implements the UpdateMetadata call of the actuator interface
func (a *GCPActuator) UpdateMetadata() error {
	// Nothing to do here since GCP CloudDNS doesn't support tags.
	return nil
}

// ModifyStatus implements the ModifyStatus call of the actuator interface
func (a *GCPActuator) ModifyStatus() error {
	if a.managedZone == nil {
		return errors.New("managedZone is unpopulated")
	}

	a.dnsZone.Status.GCP = &hivev1.GCPDNSZoneStatus{
		ZoneName: &a.managedZone.Name,
	}

	return nil
}

// GetNameServers implements the GetNameServers call of the actuator interface
func (a *GCPActuator) GetNameServers() ([]string, error) {
	if a.managedZone == nil {
		return nil, errors.New("managedZone is unpopulated")
	}

	logger := a.logger.WithField("zone", a.dnsZone.Spec.Zone)
	result := a.managedZone.NameServers
	logger.WithField("nameservers", result).Debug("found managed zone name servers")
	return result, nil
}

// Refresh implements the Refresh call of the actuator interface
func (a *GCPActuator) Refresh() error {
	var zoneName string
	if a.dnsZone.Status.GCP != nil && a.dnsZone.Status.GCP.ZoneName != nil {
		a.logger.Debug("ZoneName is set in status, will retrieve by that name")
		zoneName = *a.dnsZone.Status.GCP.ZoneName
	}

	if len(zoneName) == 0 {
		a.logger.Debug("Zone Name is not set in status, looking up by generated name")
		zoneName = generateManagedZoneName(a.dnsZone.Spec.Zone)
	}

	// Fetch the managed zone
	logger := a.logger.WithField("zoneName", zoneName)
	logger.Debug("Fetching managed zone by zone name")
	resp, err := a.gcpClient.GetManagedZone(zoneName)
	if err != nil {
		if gerr, ok := err.(*googleapi.Error); ok {
			if gerr.Code == http.StatusNotFound {
				logger.Debug("Zone not found, clearing out the cached object")
				a.managedZone = nil
				return nil
			}
		}

		logger.WithError(err).Error("Cannot get managed zone")
		return err
	}

	logger.Debug("Found managed zone")
	a.managedZone = resp
	return nil
}

// SetConditionsForError sets conditions on the dnszone given a specific error. Returns true if conditions changed.
func (a *GCPActuator) SetConditionsForError(err error) bool {
	return false // Not implemented for GCP yet.
}

func generateManagedZoneName(zone string) string {
	tmp := strings.ToLower(zone)
	tmp = strings.ReplaceAll(tmp, ".", "-")
	return "hive-" + tmp
}
