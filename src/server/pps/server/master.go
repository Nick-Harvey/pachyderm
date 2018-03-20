package server

import (
	"context"
	"fmt"
	"path"
	"time"

	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kube_watch "k8s.io/apimachinery/pkg/watch"
	kube "k8s.io/client-go/kubernetes"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/pachyderm/pachyderm/src/client/version"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/deploy/assets"
	"github.com/pachyderm/pachyderm/src/server/pkg/dlock"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	"github.com/pachyderm/pachyderm/src/server/pkg/watch"
)

const (
	masterLockPath = "_master_lock"
)

var (
	failures = map[string]bool{
		"InvalidImageName": true,
		"ErrImagePull":     true,
	}
)

// The master process is responsible for creating/deleting workers as
// pipelines are created/removed.
func (a *apiServer) master() {
	masterLock := dlock.NewDLock(a.etcdClient, path.Join(a.etcdPrefix, masterLockPath))
	backoff.RetryNotify(func() error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Use the PPS token to authenticate requests. Note that all requests
		// performed in this function are performed as a cluster admin, so do not
		// pass any unvalidated user input to any requests
		pachClient := a.getPachClient().WithCtx(ctx)
		ctx, err := masterLock.Lock(ctx)
		if err != nil {
			return err
		}
		defer masterLock.Unlock(ctx)

		log.Infof("Launching PPS master process")

		pipelineWatcher, err := a.pipelines.ReadOnly(ctx).WatchWithPrev()
		if err != nil {
			return fmt.Errorf("error creating watch: %+v", err)
		}
		defer pipelineWatcher.Close()

		// watchChan will be nil if the Watch call below errors, this means
		// that we won't receive events from k8s and won't be able to detect
		// errors in pods. We could just return that error and retry but that
		// prevents pachyderm from creating pipelines when there's an issue
		// talking to k8s.
		var watchChan <-chan kube_watch.Event
		kubePipelineWatch, err := a.kubeClient.CoreV1().Pods(a.namespace).Watch(
			metav1.ListOptions{
				LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(
					map[string]string{
						"component": "worker",
					})),
				Watch: true,
			})
		if err != nil {
			log.Errorf("failed to watch kuburnetes pods: %v", err)
		} else {
			watchChan = kubePipelineWatch.ResultChan()
			defer kubePipelineWatch.Stop()
		}

		for {
			select {
			case event := <-pipelineWatcher.Watch():
				if event.Err != nil {
					return fmt.Errorf("event err: %+v", event.Err)
				}
				switch event.Type {
				case watch.EventPut:
					var pipelineName string
					var pipelinePtr pps.EtcdPipelineInfo
					if err := event.Unmarshal(&pipelineName, &pipelinePtr); err != nil {
						return err
					}
					// Retrieve pipelineInfo (and prev pipeline's pipelineInfo) from the
					// spec repo
					var prevPipelinePtr pps.EtcdPipelineInfo
					var pipelineInfo, prevPipelineInfo *pps.PipelineInfo
					if err := a.sudo(pachClient, func(superUserClient *client.APIClient) error {
						var err error
						pipelineInfo, err = ppsutil.GetPipelineInfo(superUserClient, &pipelinePtr)
						if err != nil {
							return err
						}

						if event.PrevKey != nil {
							if err := event.UnmarshalPrev(&pipelineName, &prevPipelinePtr); err != nil {
								return err
							}
							prevPipelineInfo, err = ppsutil.GetPipelineInfo(superUserClient, &prevPipelinePtr)
							if err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						return fmt.Errorf("watch event had no pipelineInfo: %v", err)
					}

					// If the pipeline has been stopped, delete workers
					if pipelineStateToStopped(pipelinePtr.State) {
						log.Infof("PPS master: deleting workers for pipeline %s (%s)", pipelineName, pipelinePtr.State.String())
						if err := a.deleteWorkersForPipeline(pipelineInfo); err != nil {
							return err
						}
					}

					var hasGitInput bool
					pps.VisitInput(pipelineInfo.Input, func(input *pps.Input) {
						if input.Git != nil {
							hasGitInput = true
						}
					})

					// If the pipeline has been restarted, create workers
					if !pipelineStateToStopped(pipelinePtr.State) && event.PrevKey != nil && pipelineStateToStopped(prevPipelinePtr.State) {
						if hasGitInput {
							if err := a.checkOrDeployGithookService(); err != nil {
								return err
							}
						}
						log.Infof("PPS master: creating/updating workers for restarted pipeline %s", pipelineName)
						if err := a.upsertWorkersForPipeline(pipelineInfo); err != nil {
							if err := a.setPipelineFailure(ctx, pipelineName, fmt.Sprintf("failed to create workers: %s", err.Error())); err != nil {
								return err
							}
							continue
						}
					}

					// If the pipeline has been created or updated, create new workers
					pipelineUpserted := prevPipelinePtr.SpecCommit == nil ||
						pipelinePtr.SpecCommit.ID != prevPipelinePtr.SpecCommit.ID
					if pipelineUpserted && !pipelineStateToStopped(pipelinePtr.State) {
						log.Infof("PPS master: creating/updating workers for new/updated pipeline %s", pipelineName)
						if event.PrevKey != nil {
							if err := a.deleteWorkersForPipeline(prevPipelineInfo); err != nil {
								return err
							}
						}
						if hasGitInput {
							if err := a.checkOrDeployGithookService(); err != nil {
								return err
							}
						}
						if err := a.upsertWorkersForPipeline(pipelineInfo); err != nil {
							if err := a.setPipelineFailure(ctx, pipelineName, fmt.Sprintf("failed to create workers: %s", err.Error())); err != nil {
								return err
							}
							continue
						}
					}
					if pipelineInfo.State == pps.PipelineState_PIPELINE_RUNNING {
						if err := a.scaleUpWorkersForPipeline(pipelineInfo); err != nil {
							return err
						}
					}
					if pipelineInfo.State == pps.PipelineState_PIPELINE_STANDBY {
						if err := a.scaleDownWorkersForPipeline(pipelineInfo); err != nil {
							return err
						}
					}
				}
			case event := <-watchChan:
				// if we get an error we restart the watch, k8s watches seem to
				// sometimes get stuck in a loop returning events with Type =
				// "" we treat these as errors since otherwise we get an
				// endless stream of them and can't do anything.
				if event.Type == kube_watch.Error || event.Type == "" {
					if kubePipelineWatch != nil {
						kubePipelineWatch.Stop()
					}
					kubePipelineWatch, err = a.kubeClient.CoreV1().Pods(a.namespace).Watch(
						metav1.ListOptions{
							LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(
								map[string]string{
									"component": "worker",
								})),
							Watch: true,
						})
					if err != nil {
						log.Errorf("failed to watch kuburnetes pods: %v", err)
						watchChan = nil
					} else {
						watchChan = kubePipelineWatch.ResultChan()
						defer kubePipelineWatch.Stop()
					}
				}
				pod, ok := event.Object.(*v1.Pod)
				if !ok {
					continue
				}
				if pod.Status.Phase == v1.PodFailed {
					log.Errorf("pod failed because: %s", pod.Status.Message)
				}
				for _, status := range pod.Status.ContainerStatuses {
					if status.Name == "user" && status.State.Waiting != nil && failures[status.State.Waiting.Reason] {
						if err := a.setPipelineFailure(ctx, pod.ObjectMeta.Annotations["pipelineName"], status.State.Waiting.Message); err != nil {
							return err
						}
					}
				}
			}
		}
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		log.Errorf("master: error running the master process: %v; retrying in %v", err, d)
		return nil
	})
}

func (a *apiServer) setPipelineFailure(ctx context.Context, pipelineName string, reason string) error {
	return ppsutil.FailPipeline(ctx, a.etcdClient, a.pipelines, pipelineName, reason)
}

func (a *apiServer) checkOrDeployGithookService() error {
	_, err := getGithookService(a.kubeClient, a.namespace)
	if err != nil {
		if _, ok := err.(*errGithookServiceNotFound); ok {
			svc := assets.GithookService(a.namespace)
			_, err = a.kubeClient.CoreV1().Services(a.namespace).Create(svc)
			return err
		}
		return err
	}
	// service already exists
	return nil
}

func getGithookService(kubeClient *kube.Clientset, namespace string) (*v1.Service, error) {
	labels := map[string]string{
		"app":   "githook",
		"suite": suite,
	}
	serviceList, err := kubeClient.CoreV1().Services(namespace).List(metav1.ListOptions{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ListOptions",
			APIVersion: "v1",
		},
		LabelSelector: metav1.FormatLabelSelector(metav1.SetAsLabelSelector(labels)),
	})
	if err != nil {
		return nil, err
	}
	if len(serviceList.Items) != 1 {
		return nil, &errGithookServiceNotFound{
			fmt.Errorf("expected 1 githook service but found %v", len(serviceList.Items)),
		}
	}
	return &serviceList.Items[0], nil
}

func (a *apiServer) upsertWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	var errCount int
	if err := backoff.RetryNotify(func() error {
		var resourceRequests *v1.ResourceList
		var resourceLimits *v1.ResourceList
		if pipelineInfo.ResourceRequests != nil {
			var err error
			resourceRequests, err = ppsutil.GetRequestsResourceListFromPipeline(pipelineInfo)
			if err != nil {
				return err
			}
		}
		if pipelineInfo.ResourceLimits != nil {
			var err error
			resourceLimits, err = ppsutil.GetLimitsResourceListFromPipeline(pipelineInfo)
			if err != nil {
				return err
			}
		}

		// Retrieve the current state of the RC.  If the RC is scaled down,
		// we want to ensure that it remains scaled down.
		rc := a.kubeClient.CoreV1().ReplicationControllers(a.namespace)
		workerRc, err := rc.Get(
			ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version),
			metav1.GetOptions{})
		if err != nil {
			log.Errorf("error from rc.Get: %v", err)
		}
		// TODO figure out why the statement below runs even if there's an error
		// rc was made by a previous version of pachyderm so we delete it
		if workerRc.ObjectMeta.Labels["version"] != version.PrettyVersion() {
			if err := a.deleteWorkersForPipeline(pipelineInfo); err != nil {
				return err
			}
		}

		options := a.getWorkerOptions(
			pipelineInfo.Pipeline.Name,
			pipelineInfo.Version,
			0,
			resourceRequests,
			resourceLimits,
			pipelineInfo.Transform,
			pipelineInfo.CacheSize,
			pipelineInfo.Service,
			pipelineInfo.SpecCommit.ID)
		// Set the pipeline name env
		options.workerEnv = append(options.workerEnv, v1.EnvVar{
			Name:  client.PPSPipelineNameEnv,
			Value: pipelineInfo.Pipeline.Name,
		})
		return a.createWorkerRc(options)
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		errCount++
		if errCount >= 3 {
			return err
		}
		log.Errorf("error creating workers for pipeline %v: %v; retrying in %v", pipelineInfo.Pipeline.Name, err, d)
		return nil
	}); err != nil {
		return err
	}
	if _, ok := a.monitorCancels[pipelineInfo.Pipeline.Name]; !ok {
		ctx, cancel := context.WithCancel(a.pachClient.Ctx())
		a.monitorCancels[pipelineInfo.Pipeline.Name] = cancel
		pachClient := a.pachClient.WithCtx(ctx)
		go a.monitorPipeline(pachClient, pipelineInfo)
	}
	return nil
}

func (a *apiServer) deleteWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	cancel, ok := a.monitorCancels[pipelineInfo.Pipeline.Name]
	if ok {
		cancel()
		delete(a.monitorCancels, pipelineInfo.Pipeline.Name)
	}
	rcName := ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version)
	if err := a.kubeClient.CoreV1().Services(a.namespace).Delete(
		rcName, &metav1.DeleteOptions{},
	); err != nil {
		if !isNotFoundErr(err) {
			return err
		}
	}
	if pipelineInfo.Service != nil {
		if err := a.kubeClient.CoreV1().Services(a.namespace).Delete(
			rcName+"-user", &metav1.DeleteOptions{},
		); err != nil {
			if !isNotFoundErr(err) {
				return err
			}
		}
	}
	falseVal := false
	deleteOptions := &metav1.DeleteOptions{
		OrphanDependents: &falseVal,
	}
	if err := a.kubeClient.CoreV1().ReplicationControllers(a.namespace).Delete(rcName, deleteOptions); err != nil {
		if !isNotFoundErr(err) {
			return err
		}
	}
	return nil
}

func (a *apiServer) scaleDownWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	rc := a.kubeClient.CoreV1().ReplicationControllers(a.namespace)
	workerRc, err := rc.Get(
		ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version),
		metav1.GetOptions{})
	if err != nil {
		return err
	}
	*workerRc.Spec.Replicas = 0
	_, err = rc.Update(workerRc)
	return err
}

func (a *apiServer) scaleUpWorkersForPipeline(pipelineInfo *pps.PipelineInfo) error {
	rc := a.kubeClient.CoreV1().ReplicationControllers(a.namespace)
	workerRc, err := rc.Get(
		ppsutil.PipelineRcName(pipelineInfo.Pipeline.Name, pipelineInfo.Version),
		metav1.GetOptions{})
	if err != nil {
		return err
	}
	parallelism, err := ppsutil.GetExpectedNumWorkers(a.kubeClient, pipelineInfo.ParallelismSpec)
	if err != nil {
		log.Errorf("error getting number of workers, default to 1 worker: %v", err)
		parallelism = 1
	}
	*workerRc.Spec.Replicas = int32(parallelism)
	_, err = rc.Update(workerRc)
	return err
}

func (a *apiServer) monitorPipeline(pachClient *client.APIClient, pipelineInfo *pps.PipelineInfo) {
	backoff.RetryNotify(func() error {
		ciChan := make(chan *pfs.CommitInfo)
		go func() {
			if err := pachClient.SubscribeCommitF(pipelineInfo.Pipeline.Name, pipelineInfo.OutputBranch, "", pfs.CommitState_READY, func(ci *pfs.CommitInfo) error {
				fmt.Printf("got ci for %s\n", pipelineInfo.Pipeline.Name)
				ciChan <- ci
				return nil
			}); err != nil {
				fmt.Printf("error from SubscribeCommit in monitorPipeline: %v\n", err)
			}
		}()
		// standbyChan is used in the select below to figure out if we should go into standby
		// it starts closed, which means that `case <-standbyChan:` is equivalent to `default:`
		// When we go into standby it gets set to nil which means `case <-standbyChan` is equivalent to not having that case
		// When we exit standby we reset it to a closed channel so that it again behaves like `default:`
		standbyChan := make(chan struct{})
		close(standbyChan)
		for {
			select {
			case ci := <-ciChan:
				if ci.Finished != nil {
					// The commit has been finished which means the job is
					// likely complete, however we must check that explicitly
					// because there's a gap between when the commit gets
					// finished and the job completes. This gap is normally
					// small but can be large in the case of eggress.
					jobInfo, err := pachClient.InspectJobOutputCommit(ci.Commit.Repo.Name, ci.Commit.ID, false)
					if err != nil {
						return err
					}
					if ppsutil.IsTerminal(jobInfo.State) {
						continue
					}
				}
				if _, err := col.NewSTM(pachClient.Ctx(), a.etcdClient, func(stm col.STM) error {
					pipelines := a.pipelines.ReadWrite(stm)
					pipelinePtr := &pps.EtcdPipelineInfo{}
					return pipelines.Upsert(pipelineInfo.Pipeline.Name, pipelinePtr, func() error {
						if pipelinePtr.State == pps.PipelineState_PIPELINE_PAUSED {
							return nil
						}
						pipelinePtr.State = pps.PipelineState_PIPELINE_RUNNING
						return nil
					})
				}); err != nil {
					return err
				}
				standbyChan = make(chan struct{})
				close(standbyChan)
				// Wait for the commit to be finished before blocking on the
				// job because the job may not exist yet.
				if _, err := pachClient.BlockCommit(ci.Commit.Repo.Name, ci.Commit.ID); err != nil {
					return err
				}
				if _, err := pachClient.InspectJobOutputCommit(ci.Commit.Repo.Name, ci.Commit.ID, true); err != nil {
					return err
				}
			case <-standbyChan:
				if _, err := col.NewSTM(pachClient.Ctx(), a.etcdClient, func(stm col.STM) error {
					pipelines := a.pipelines.ReadWrite(stm)
					pipelinePtr := &pps.EtcdPipelineInfo{}
					return pipelines.Upsert(pipelineInfo.Pipeline.Name, pipelinePtr, func() error {
						if pipelinePtr.State == pps.PipelineState_PIPELINE_PAUSED {
							return nil
						}
						pipelinePtr.State = pps.PipelineState_PIPELINE_STANDBY
						return nil
					})
				}); err != nil {
					return err
				}
				// set standbyChan to nil so we won't enter this case until it's reset
				standbyChan = nil
			}
		}
		return nil
	}, backoff.NewInfiniteBackOff(), func(err error, d time.Duration) error {
		select {
		case <-pachClient.Ctx().Done():
			return context.DeadlineExceeded
		default:
			fmt.Printf("error in monitorPipeline: %v: retrying in: %v\n", err, d)
		}
		return nil
	})
}
