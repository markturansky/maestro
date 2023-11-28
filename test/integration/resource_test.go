package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gopkg.in/resty.v1"
	"k8s.io/apimachinery/pkg/labels"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift-online/maestro/pkg/api"
	"github.com/openshift-online/maestro/pkg/api/openapi"
	"github.com/openshift-online/maestro/pkg/client/cloudevents"
	"github.com/openshift-online/maestro/pkg/dao"
	"github.com/openshift-online/maestro/test"
	workv1 "open-cluster-management.io/api/work/v1"
)

func TestResourceGet(t *testing.T) {
	h, client := test.RegisterIntegration(t)

	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account)

	// 401 using no JWT token
	_, _, err := client.DefaultApi.ApiMaestroV1ResourcesIdGet(context.Background(), "foo").Execute()
	Expect(err).To(HaveOccurred(), "Expected 401 but got nil error")

	// GET responses per openapi spec: 200 and 404,
	_, resp, err := client.DefaultApi.ApiMaestroV1ResourcesIdGet(ctx, "foo").Execute()
	Expect(err).To(HaveOccurred(), "Expected 404")
	Expect(resp.StatusCode).To(Equal(http.StatusNotFound))

	consumer := h.NewConsumer("cluster1")
	res := h.NewResource("cluster1", 1, 1)

	resource, resp, err := client.DefaultApi.ApiMaestroV1ResourcesIdGet(ctx, res.ID).Execute()
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	Expect(*resource.Id).To(Equal(res.ID), "found object does not match test object")
	Expect(*resource.Kind).To(Equal("Resource"))
	Expect(*resource.Href).To(Equal(fmt.Sprintf("/api/maestro/v1/resources/%s", res.ID)))
	Expect(*resource.CreatedAt).To(BeTemporally("~", res.CreatedAt))
	Expect(*resource.UpdatedAt).To(BeTemporally("~", res.UpdatedAt))
}

func TestResourcePost(t *testing.T) {
	h, client := test.RegisterIntegration(t)
	account := h.NewRandAccount()
	ctx, cancel := context.WithCancel(h.NewAuthenticatedContext(account))

	clusterName := "cluster1"
	h.StartControllerManager(ctx)
	h.StartWorkAgent(ctx, clusterName, h.Env().Config.MessageBroker.MQTTOptions)
	clientHolder := h.WorkAgentHolder
	informer := clientHolder.ManifestWorkInformer()
	lister := informer.Lister().ManifestWorks(clusterName)
	agentWorkClient := clientHolder.ManifestWorks(clusterName)
	resourceService := h.Env().Services.Resources()
	sourceClient := h.Env().Clients.CloudEventsSource

	// POST responses per openapi spec: 201, 409, 500
	consumer := h.NewConsumer("cluster1")
	res := h.NewAPIResource(consumer.ID, 1, 1)

	// 201 Created
	resource, resp, err := client.DefaultApi.ApiMaestroV1ResourcesPost(ctx).Resource(res).Execute()
	Expect(err).NotTo(HaveOccurred(), "Error posting object:  %v", err)
	Expect(resp.StatusCode).To(Equal(http.StatusCreated))
	Expect(*resource.Id).NotTo(BeEmpty(), "Expected ID assigned on creation")
	Expect(*resource.Kind).To(Equal("Resource"))
	Expect(*resource.Href).To(Equal(fmt.Sprintf("/api/maestro/v1/resources/%s", *resource.Id)))

	// 400 bad request. posting junk json is one way to trigger 400.
	jwtToken := ctx.Value(openapi.ContextAccessToken)
	restyResp, err := resty.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", fmt.Sprintf("Bearer %s", jwtToken)).
		SetBody(`{ this is invalid }`).
		Post(h.RestURL("/resources"))

	Expect(err).NotTo(HaveOccurred(), "Error posting object:  %v", err)
	Expect(restyResp.StatusCode()).To(Equal(http.StatusBadRequest))

	var work *workv1.ManifestWork
	Eventually(func() error {
		list, err := lister.List(labels.Everything())
		if err != nil {
			return err
		}

		// ensure there is only one work was synced on the cluster
		if len(list) != 1 {
			return fmt.Errorf("unexpected work list %v", list)
		}

		// ensure the work can be get by work client
		work, err = agentWorkClient.Get(ctx, *resource.Id, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(Succeed())

	Expect(work).NotTo(BeNil())
	Expect(work.Spec.Workload).NotTo(BeNil())
	Expect(len(work.Spec.Workload.Manifests)).To(Equal(1))
	manifest := map[string]interface{}{}
	Expect(json.Unmarshal(work.Spec.Workload.Manifests[0].Raw, &manifest)).NotTo(HaveOccurred(), "Error unmarshalling manifest:  %v", err)
	Expect(manifest).To(Equal(res.Manifest))

	newWork := work.DeepCopy()
	newWork.Status = workv1.ManifestWorkStatus{Conditions: []metav1.Condition{{Type: "Applied", Status: metav1.ConditionTrue}}}
	// only update the status on the agent local part
	Expect(informer.Informer().GetStore().Update(newWork)).NotTo(HaveOccurred())

	// Resync the resource status
	ceSourceClient, ok := sourceClient.(*cloudevents.SourceClientImpl)
	Expect(ok).To(BeTrue())
	Expect(ceSourceClient.CloudEventSourceClient.Resync(ctx)).NotTo(HaveOccurred())

	Eventually(func() error {
		newRes, err := resourceService.Get(ctx, *resource.Id)
		if err != nil {
			return err
		}
		if newRes.Status == nil || len(newRes.Status) == 0 {
			return fmt.Errorf("resource status is empty")
		}
		return nil
	}, 10*time.Second, 1*time.Second).Should(Succeed())

	newRes, err := resourceService.Get(ctx, *resource.Id)
	Expect(err).NotTo(HaveOccurred(), "Error getting resource: %v", err)
	Expect(newRes.Status["ReconcileStatus"]).NotTo(BeNil())
	conditions := newRes.Status["ReconcileStatus"].(map[string]interface{})["Conditions"].([]interface{})
	Expect(len(conditions)).To(Equal(1))
	condition := conditions[0].(map[string]interface{})
	Expect(condition["type"]).To(Equal("Applied"))
	Expect(condition["status"]).To(Equal("True"))

	// make sure controller manager and work agent are stopped
	cancel()
}

func TestResourcePatch(t *testing.T) {
	h, client := test.RegisterIntegration(t)
	account := h.NewRandAccount()
	ctx, cancel := context.WithCancel(h.NewAuthenticatedContext(account))

	clusterName := "cluster1"
	consumer := h.NewConsumer(clusterName)
	h.StartControllerManager(ctx)
	h.StartWorkAgent(ctx, clusterName, h.Env().Config.MessageBroker.MQTTOptions)
	clientHolder := h.WorkAgentHolder
	informer := clientHolder.ManifestWorkInformer()
	lister := informer.Lister().ManifestWorks(clusterName)
	agentWorkClient := clientHolder.ManifestWorks(clusterName)

	// POST responses per openapi spec: 201, 409, 500
	res := h.NewResource(consumer.ID , 1, 1)
	newRes := h.NewAPIResource(consumer.ID, 2, 2)

	// 200 OK
	resource, resp, err := client.DefaultApi.ApiMaestroV1ResourcesIdPatch(ctx, res.ID).ResourcePatchRequest(openapi.ResourcePatchRequest{Manifest: newRes.Manifest}).Execute()
	Expect(err).NotTo(HaveOccurred(), "Error posting object:  %v", err)
	Expect(resp.StatusCode).To(Equal(http.StatusOK))
	Expect(*resource.Id).To(Equal(res.ID))
	Expect(*resource.CreatedAt).To(BeTemporally("~", res.CreatedAt))
	Expect(*resource.Kind).To(Equal("Resource"))
	Expect(*resource.Href).To(Equal(fmt.Sprintf("/api/maestro/v1/resources/%s", *resource.Id)))
	Expect(*resource.Version).To(Equal(res.Version + 1))

	jwtToken := ctx.Value(openapi.ContextAccessToken)
	// 500 server error. posting junk json is one way to trigger 500.
	restyResp, err := resty.R().
		SetHeader("Content-Type", "application/json").
		SetHeader("Authorization", fmt.Sprintf("Bearer %s", jwtToken)).
		SetBody(`{ this is invalid }`).
		Patch(h.RestURL("/resources/foo"))

	Expect(err).NotTo(HaveOccurred(), "Error posting object:  %v", err)
	Expect(restyResp.StatusCode()).To(Equal(http.StatusBadRequest))

	dao := dao.NewEventDao(&h.Env().Database.SessionFactory)
	events, err := dao.All(ctx)
	Expect(err).NotTo(HaveOccurred(), "Error getting events:  %v", err)
	Expect(len(events)).To(Equal(2), "expected Create and Update events")
	Expect(contains(api.CreateEventType, events)).To(BeTrue())
	Expect(contains(api.UpdateEventType, events)).To(BeTrue())

	var work *workv1.ManifestWork
	Eventually(func() error {
		list, err := lister.List(labels.Everything())
		if err != nil {
			return err
		}

		// ensure there is only one work was synced on the cluster
		if len(list) != 1 {
			return fmt.Errorf("unexpected work list %v", list)
		}

		// ensure the work can be get by work client
		work, err = agentWorkClient.Get(ctx, *resource.Id, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// ensure the work version is updated
		if work.GetResourceVersion() != "2" {
			return fmt.Errorf("unexpected work version %v", work.GetResourceVersion())
		}

		return nil
	}, 10*time.Second, 1*time.Second).Should(Succeed())

	Expect(work).NotTo(BeNil())
	Expect(work.Spec.Workload).NotTo(BeNil())
	Expect(len(work.Spec.Workload.Manifests)).To(Equal(1))
	manifest := map[string]interface{}{}
	Expect(json.Unmarshal(work.Spec.Workload.Manifests[0].Raw, &manifest)).NotTo(HaveOccurred(), "Error unmarshalling manifest:  %v", err)
	Expect(manifest).To(Equal(newRes.Manifest))

	// make sure controller manager and work agent are stopped
	cancel()
}

func contains(et api.EventType, events api.EventList) bool {
	for _, e := range events {
		if e.EventType == et {
			return true
		}
	}
	return false
}

func TestResourcePaging(t *testing.T) {
	h, client := test.RegisterIntegration(t)

	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account)

	// Paging
	consumer := h.NewConsumer("cluster1")
	_ = h.NewResourceList(consumer.ID, 20)

	list, _, err := client.DefaultApi.ApiMaestroV1ResourcesGet(ctx).Execute()
	Expect(err).NotTo(HaveOccurred(), "Error getting resource list: %v", err)
	Expect(len(list.Items)).To(Equal(20))
	Expect(list.Size).To(Equal(int32(20)))
	Expect(list.Total).To(Equal(int32(20)))
	Expect(list.Page).To(Equal(int32(1)))

	list, _, err = client.DefaultApi.ApiMaestroV1ResourcesGet(ctx).Page(2).Size(5).Execute()
	Expect(err).NotTo(HaveOccurred(), "Error getting resource list: %v", err)
	Expect(len(list.Items)).To(Equal(5))
	Expect(list.Size).To(Equal(int32(5)))
	Expect(list.Total).To(Equal(int32(20)))
	Expect(list.Page).To(Equal(int32(2)))
}

func TestResourceListSearch(t *testing.T) {
	h, client := test.RegisterIntegration(t)

	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account)

	consumer := h.NewConsumer("cluster1")
	resources := h.NewResourceList(consumer.ID, 20)

	search := fmt.Sprintf("id in ('%s')", resources[0].ID)
	list, _, err := client.DefaultApi.ApiMaestroV1ResourcesGet(ctx).Search(search).Execute()
	Expect(err).NotTo(HaveOccurred(), "Error getting resource list: %v", err)
	Expect(len(list.Items)).To(Equal(1))
	Expect(list.Total).To(Equal(int32(1)))
	Expect(*list.Items[0].Id).To(Equal(resources[0].ID))
}
