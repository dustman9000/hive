package installmanager

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1"
	awsclient "github.com/openshift/hive/pkg/awsclient"
	dns "github.com/openshift/hive/pkg/controller/dnszone"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
)

// cleanupDNSZone will handle any needed DNS cleanup for ClusterDeployments with
// ManageDNS enabled (this helps to clean up any stray DNS records on install failures)
func cleanupDNSZone(dynClient client.Client, cd *hivev1.ClusterDeployment, logger log.FieldLogger) error {
	if cd.Spec.ManageDNS == false {
		return nil
	}

	dnsZone := &hivev1.DNSZone{}
	dnsZoneNamespacedName := types.NamespacedName{Namespace: cd.Namespace, Name: controllerutils.DNSZoneName(cd.Name)}
	if err := dynClient.Get(context.TODO(), dnsZoneNamespacedName, dnsZone); err != nil {
		logger.WithError(err).Error("error looking up managed dnszone")
	}

	switch {
	case cd.Spec.Platform.AWS != nil:
		return cleanupAWSDNSZone(dnsZone, cd.Spec.Platform.AWS.Region, logger)
	default:
		log.Debug("No DNS cleanup for platform type")
		return nil
	}
}

// cleanupAWSDNSZone will return a DNS zone to the minimum set of DNS records
// May no longer be necessary once https://jira.coreos.com/browse/CORS-1195 is fixed.
func cleanupAWSDNSZone(dnsZone *hivev1.DNSZone, region string, logger log.FieldLogger) error {
	if dnsZone.Status.AWS == nil {
		return fmt.Errorf("found non-AWS DNSZone for AWS ClusterDeployment")
	}
	if dnsZone.Status.AWS.ZoneID == nil {
		// Shouldn't really be possible as we block install until DNS is ready:
		return fmt.Errorf("DNSZone %s has no ZoneID set", dnsZone.Name)
	}

	zoneLogger := logger.WithField("dnsZoneID", *dnsZone.Status.AWS.ZoneID)
	zoneLogger.Info("cleaning up DNSZone")

	awsClient, err := awsclient.NewClient(nil, "", "", region)
	if err != nil {
		return err
	}

	if err := dns.DeleteAWSRecordSets(awsClient, dnsZone, zoneLogger); err != nil {
		logger.WithError(err).Error("failed to clean up DNS Zone")
		return err
	}
	zoneLogger.Info("DNSZone cleaned")
	return nil
}
