package watch

import (
	"k8s.io/client-go/kubernetes"
	kubeapi "k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/labels"
	"k8s.io/client-go/pkg/util/workqueue"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	kvirtv1 "kubevirt.io/kubevirt/pkg/api/v1"
	"kubevirt.io/kubevirt/pkg/kubecli"
	"kubevirt.io/kubevirt/pkg/virt-controller/services"
)

func migrationJobSelector() kubeapi.ListOptions {
	fieldSelector := fields.ParseSelectorOrDie(
		"status.phase!=" + string(v1.PodPending) +
			",status.phase!=" + string(v1.PodRunning) +
			",status.phase!=" + string(v1.PodUnknown))
	labelSelector, err := labels.Parse(kvirtv1.AppLabel + "=migration," + kvirtv1.DomainLabel + "," + kvirtv1.MigrationLabel)
	if err != nil {
		panic(err)
	}
	return kubeapi.ListOptions{FieldSelector: fieldSelector, LabelSelector: labelSelector}
}

func NewJobController(vmService services.VMService, recorder record.EventRecorder, clientSet *kubernetes.Clientset, restClient *rest.RESTClient) (cache.Store, *kubecli.Controller) {
	selector := migrationJobSelector()
	lw := kubecli.NewListWatchFromClient(clientSet.CoreV1().RESTClient(), "pods", kubeapi.NamespaceDefault, selector.FieldSelector, selector.LabelSelector)
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	return kubecli.NewController(lw, queue, &v1.Pod{}, NewJobControllerDispatch(vmService, restClient))
}

func NewJobControllerDispatch(vmService services.VMService, restClient *rest.RESTClient) kubecli.ControllerDispatch {
	dispatch := JobDispatch{
		restClient: restClient,
		vmService:  vmService,
	}
	var vmd kubecli.ControllerDispatch = &dispatch
	return vmd
}

type JobDispatch struct {
	restClient *rest.RESTClient
	vmService  services.VMService
}

func (jd *JobDispatch) Execute(store cache.Store, queue workqueue.RateLimitingInterface, key interface{}) {
	obj, exists, err := store.GetByKey(key.(string))
	if err != nil {
		queue.AddRateLimited(key)
		return
	}
	if exists {
		job := obj.(*v1.Pod)

		name := job.ObjectMeta.Labels[kvirtv1.DomainLabel]
		vm, vmExists, err := jd.vmService.FetchVM(name)
		if err != nil {
			queue.AddRateLimited(key)
			return
		}

		// TODO at the end, only virt-handler can decide for all migration types if a VM successfully migrated to it (think about p2p2 migrations)
		// For now we use a managed migration
		if vmExists && vm.Status.Phase == kvirtv1.Migrating {
			vm.Status.Phase = kvirtv1.Running
			if job.Status.Phase == v1.PodSucceeded {
				vm.ObjectMeta.Labels[kvirtv1.NodeNameLabel] = vm.Status.MigrationNodeName
				vm.Status.NodeName = vm.Status.MigrationNodeName
			}
			vm.Status.MigrationNodeName = ""
			_, err := putVm(vm, jd.restClient, nil)
			if err != nil {
				queue.AddRateLimited(key)
				return
			}
		}

		migration, migrationExists, err := jd.vmService.FetchMigration(job.ObjectMeta.Labels[kvirtv1.MigrationLabel])
		if err != nil {
			queue.AddRateLimited(key)
			return
		}

		if migrationExists {
			if migration.Status.Phase != kvirtv1.MigrationSucceeded && migration.Status.Phase != kvirtv1.MigrationFailed {
				if job.Status.Phase == v1.PodSucceeded {
					migration.Status.Phase = kvirtv1.MigrationSucceeded
				} else {
					migration.Status.Phase = kvirtv1.MigrationFailed
				}
				err := jd.vmService.UpdateMigration(migration)
				if err != nil {
					queue.AddRateLimited(key)
					return
				}
			}
		}
	}
	return
}
