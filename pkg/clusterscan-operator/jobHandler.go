package clusterscan_operator

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/rancher/security-scan/pkg/kb-summarizer/report"
	reportLibrary "github.com/rancher/security-scan/pkg/kb-summarizer/report"
	batchctlv1 "github.com/rancher/wrangler/pkg/generated/controllers/batch/v1"

	"time"

	cisoperatorapi "github.com/rancher/clusterscan-operator/pkg/apis/clusterscan-operator.cattle.io"
	v1 "github.com/rancher/clusterscan-operator/pkg/apis/clusterscan-operator.cattle.io/v1"
	"github.com/rancher/wrangler/pkg/name"
)

// job events (successful completions) should remove the job after validatinf Done annotation and Output CM
func (c *Controller) handleJobs(ctx context.Context) error {
	scans := c.cisFactory.Clusterscanoperator().V1().ClusterScan()
	reports := c.cisFactory.Clusterscanoperator().V1().ClusterScanReport()
	jobs := c.batchFactory.Batch().V1().Job()

	jobs.OnChange(ctx, c.Name, func(key string, obj *batchv1.Job) (*batchv1.Job, error) {
		if obj == nil || obj.DeletionTimestamp != nil {
			return obj, nil
		}
		jobSelector := labels.SelectorFromSet(labels.Set{
			cisoperatorapi.LabelController: c.Name,
		})
		// avoid commandeering jobs from other controllers
		if obj.Labels == nil || !jobSelector.Matches(labels.Set(obj.Labels)) {
			return obj, nil
		}
		// identify the clusterscan object for this job
		scanName, ok := obj.Labels[cisoperatorapi.LabelClusterScan]
		if !ok {
			// malformed, just delete it and move on
			logrus.Errorf("malformed scan, deleting the job %v", obj.Name)
			return obj, c.deleteJob(jobs, obj, metav1.DeletePropagationBackground)
		}
		// get the scan being run
		scan, err := scans.Get(scanName, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			// scan is gone, delete
			logrus.Errorf("scan gone, deleting the job %v", obj.Name)
			return obj, c.deleteJob(jobs, obj, metav1.DeletePropagationBackground)
		case err != nil:
			return obj, err
		}

		// if the scan has completed then delete the job
		if v1.ClusterScanConditionComplete.IsTrue(scan) {
			v1.ClusterScanConditionAlerted.Unknown(scan)
			scan.Status.ObservedGeneration = scan.Generation
			logrus.Infof("Marking ClusterScanConditionAlerted for clusterscan: %v", scanName)
			//update scan
			_, err = scans.UpdateStatus(scan)
			if err != nil {
				return nil, fmt.Errorf("error updating condition of cluster scan object: %v", scanName)
			}
			return obj, c.deleteJob(jobs, obj, metav1.DeletePropagationBackground)
		}

		if v1.ClusterScanConditionRunCompleted.IsTrue(scan) {
			scancopy := scan.DeepCopy()

			if !v1.ClusterScanConditionFailed.IsTrue(scan) {
				summary, report, err := c.getScanResults(scan)
				if err != nil {
					return nil, fmt.Errorf("error %v reading results of cluster scan object: %v", err, scanName)
				}
				scancopy.Status.Summary = summary
				err = c.apply.WithCacheTypes(reports).ApplyObjects(report)
				if err != nil {
					return nil, fmt.Errorf("error %v saving clusterscanreport object", err)
				}
			}
			v1.ClusterScanConditionComplete.True(scancopy)
			/* update scan */
			_, err = scans.UpdateStatus(scancopy)
			if err != nil {
				return nil, fmt.Errorf("error updating condition of clusterscan object: %v", scanName)
			}
			logrus.Infof("Marking ClusterScanConditionComplete for clusterscan: %v", scanName)
			jobs.Enqueue(obj.Namespace, obj.Name)
		}
		return obj, nil
	})
	return nil
}

func (c *Controller) deleteJob(jobController batchctlv1.JobController, job *batchv1.Job, deletionPropagation metav1.DeletionPropagation) error {
	return jobController.Delete(job.Namespace, job.Name, &metav1.DeleteOptions{PropagationPolicy: &deletionPropagation})
}

func (c *Controller) getScanResults(scan *v1.ClusterScan) (*v1.ClusterScanSummary, *v1.ClusterScanReport, error) {
	configmaps := c.coreFactory.Core().V1().ConfigMap()
	//get the output configmap and create a report
	outputConfigName := strings.Join([]string{`cisscan-output-for`, scan.Name}, "-")
	cm, err := configmaps.Cache().Get(v1.ClusterScanNS, outputConfigName)
	if err != nil {
		return nil, nil, fmt.Errorf("cisScanHandler: Updated: error fetching configmap %v: %v", outputConfigName, err)
	}
	outputBytes := []byte(cm.Data[v1.DefaultScanOutputFileName])
	r, err := report.Get(outputBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("cisScanHandler: Updated: error getting report from configmap %v: %v", outputConfigName, err)
	}
	if r == nil {
		return nil, nil, fmt.Errorf("cisScanHandler: Updated: error: got empty report from configmap %v", outputConfigName)
	}
	cisScanSummary := &v1.ClusterScanSummary{
		Total:         r.Total,
		Pass:          r.Pass,
		Fail:          r.Fail,
		Skip:          r.Skip,
		NotApplicable: r.NotApplicable,
	}

	scanReport := &v1.ClusterScanReport{
		ObjectMeta: metav1.ObjectMeta{
			Name: name.SafeConcatName("clusterscan-report-", scan.Name),
		},
	}
	profile, err := c.getClusterScanProfile(scan)
	if err != nil {
		return nil, nil, fmt.Errorf("Error %v loading v1.ClusterScanProfile for name %v", scan.Spec.ScanProfileName, err)
	}
	scanReport.Spec.BenchmarkVersion = profile.Spec.BenchmarkVersion
	scanReport.Spec.LastRunTimestamp = time.Now().String()
	data, err := reportLibrary.GetJSONBytes(outputBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("Error %v loading clusterscan report json bytes", err)
	}
	scanReport.Spec.ReportJSON = string(data[:])

	return cisScanSummary, scanReport, nil
}
