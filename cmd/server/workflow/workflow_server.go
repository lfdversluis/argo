package workflow

import (
	"bufio"
	"fmt"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/argoproj/argo/cmd/server/auth"
	"github.com/argoproj/argo/persist/sqldb"
	"github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo/workflow/common"
	"github.com/argoproj/argo/workflow/packer"
	"github.com/argoproj/argo/workflow/templateresolution"
	"github.com/argoproj/argo/workflow/util"
	"github.com/argoproj/argo/workflow/validate"
)

type workflowServer struct {
	offloadNodeStatusRepo sqldb.OffloadNodeStatusRepo
	wfKubeService         *kubeService
}

func NewWorkflowServer(offloadNodeStatusRepo sqldb.OffloadNodeStatusRepo) WorkflowServiceServer {
	return &workflowServer{
		offloadNodeStatusRepo: offloadNodeStatusRepo,
	}
}

func (s *workflowServer) CreateWorkflow(ctx context.Context, req *WorkflowCreateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	if req.Workflow == nil {
		return nil, fmt.Errorf("workflow body not specified")
	}

	if req.Workflow.Namespace == "" {
		req.Workflow.Namespace = req.Namespace
	}

	if req.InstanceID != "" {
		labels := req.Workflow.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[common.LabelKeyControllerInstanceID] = req.InstanceID
		req.Workflow.SetLabels(labels)
	}

	wftmplGetter := templateresolution.WrapWorkflowTemplateInterface(wfClient.ArgoprojV1alpha1().WorkflowTemplates(req.Namespace))

	err := validate.ValidateWorkflow(wftmplGetter, req.Workflow, validate.ValidateOpts{})
	if err != nil {
		return nil, err
	}

	if req.ServerDryRun {
		return util.CreateServerDryRun(req.Workflow, wfClient)
	}

	wf, err := s.wfKubeService.Create(wfClient, req.Namespace, req.Workflow)

	if err != nil {
		log.Errorf("Create request is failed. Error: %s", err)
		return nil, err

	}
	return wf, nil
}

func (s *workflowServer) GetWorkflow(ctx context.Context, req *WorkflowGetRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	wf, err := s.wfKubeService.Get(wfClient, req.Namespace, req.WorkflowName, req.GetOptions)
	if err != nil {
		return nil, err
	}

	if wf.Status.OffloadNodeStatus {
		offloaded, err := s.offloadNodeStatusRepo.Get(req.WorkflowName, req.Namespace)
		if err != nil {
			return nil, err
		}
		wf.Status.Nodes = offloaded.Status.Nodes
		wf.Status.CompressedNodes = offloaded.Status.CompressedNodes
	}
	err = packer.DecompressWorkflow(wf)
	if err != nil {
		return nil, err
	}
	return wf, nil
}

func (s *workflowServer) ListWorkflows(ctx context.Context, req *WorkflowListRequest) (*v1alpha1.WorkflowList, error) {
	wfClient := auth.GetWfClient(ctx)

	wfList, err := s.wfKubeService.List(wfClient, req.Namespace, req)
	if err != nil {
		return nil, err
	}

	return wfList, nil
}

func (s *workflowServer) WatchWorkflows(req *WatchWorkflowsRequest, ws WorkflowService_WatchWorkflowsServer) error {
	wfClient := auth.GetWfClient(ws.Context())
	opts := metav1.ListOptions{}
	if req.ListOptions != nil {
		opts = *req.ListOptions
	}
	wfs, err := wfClient.ArgoprojV1alpha1().Workflows(req.Namespace).Watch(opts)
	if err != nil {
		return err
	}

	done := make(chan bool)
	go func() {
		for next := range wfs.ResultChan() {
			wf := next.Object.(*v1alpha1.Workflow)
			err := packer.DecompressWorkflow(wf)
			logCtx := log.WithFields(log.Fields{"type": next.Type, "namespace": wf.Namespace, "workflowName": wf.Name})
			if err != nil {
				logCtx.Warnf("Unable to send stream message: %v", err)
				break
			}
			if wf.Status.OffloadNodeStatus {
				offloaded, err := s.offloadNodeStatusRepo.Get(wf.Name, wf.Namespace)
				if err != nil {
					logCtx.Warnf("Unable to send stream message: %v", err)
					break
				}
				wf.Status.Nodes = offloaded.Status.Nodes
			}
			logCtx.Debug("Sending event")
			err = ws.Send(&WorkflowWatchEvent{Type: string(next.Type), Object: wf})
			if err != nil {
				logCtx.Warnf("Unable to send stream message: %v", err)
				break
			}
		}
		done <- true
	}()

	select {
	case <-ws.Context().Done():
		wfs.Stop()
	case <-done:
		wfs.Stop()
	}

	return nil
}

func (s *workflowServer) DeleteWorkflow(ctx context.Context, req *WorkflowDeleteRequest) (*WorkflowDeleteResponse, error) {
	wfClient := auth.GetWfClient(ctx)

	wf, err := s.wfKubeService.Get(wfClient, req.Namespace, req.WorkflowName, nil)
	if err != nil {
		return nil, err
	}

	if wf.Status.OffloadNodeStatus {
		err = s.offloadNodeStatusRepo.Delete(req.WorkflowName, req.Namespace)
		if err != nil {
			return nil, err
		}
	}

	return s.wfKubeService.Delete(wfClient, req.Namespace, req)
}

func (s *workflowServer) RetryWorkflow(ctx context.Context, req *WorkflowUpdateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	kubeClient := auth.GetKubeClient(ctx)
	return s.wfKubeService.Retry(wfClient, kubeClient, req.Namespace, req)
}

func (s *workflowServer) ResubmitWorkflow(ctx context.Context, req *WorkflowUpdateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	return s.wfKubeService.Resubmit(wfClient, req.Namespace, req)
}

func (s *workflowServer) ResumeWorkflow(ctx context.Context, req *WorkflowUpdateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	return s.wfKubeService.Resume(wfClient, req.Namespace, req)
}

func (s *workflowServer) SuspendWorkflow(ctx context.Context, req *WorkflowUpdateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	return s.wfKubeService.Suspend(wfClient, req.Namespace, req)
}

func (s *workflowServer) TerminateWorkflow(ctx context.Context, req *WorkflowUpdateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)
	return s.wfKubeService.Terminate(wfClient, req.Namespace, req)
}

func (s *workflowServer) LintWorkflow(ctx context.Context, req *WorkflowCreateRequest) (*v1alpha1.Workflow, error) {
	wfClient := auth.GetWfClient(ctx)

	wftmplGetter := templateresolution.WrapWorkflowTemplateInterface(wfClient.ArgoprojV1alpha1().WorkflowTemplates(req.Namespace))

	err := validate.ValidateWorkflow(wftmplGetter, req.Workflow, validate.ValidateOpts{})
	if err != nil {
		return nil, err
	}

	return req.Workflow, nil
}

func (s *workflowServer) PodLogs(req *WorkflowLogRequest, ws WorkflowService_PodLogsServer) error {
	kubeClient := auth.GetKubeClient(ws.Context())
	stream, err := kubeClient.CoreV1().Pods(req.Namespace).GetLogs(req.PodName, req.LogOptions).Stream()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		err = ws.Send(&LogEntry{Content: scanner.Text()})
		if err != nil {
			return err
		}
	}
	return nil
}
