package eventmanager

import (
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/types"

	"github.com/golang/mock/gomock"
	upgradev1alpha1 "github.com/openshift/managed-upgrade-operator/pkg/apis/upgrade/v1alpha1"
	configMock "github.com/openshift/managed-upgrade-operator/pkg/configmanager/mocks"
	metricsMock "github.com/openshift/managed-upgrade-operator/pkg/metrics/mocks"
	"github.com/openshift/managed-upgrade-operator/pkg/notifier"
	notifierMock "github.com/openshift/managed-upgrade-operator/pkg/notifier/mocks"
	ucMgrMock "github.com/openshift/managed-upgrade-operator/pkg/upgradeconfigmanager/mocks"
	"github.com/openshift/managed-upgrade-operator/util/mocks"
	testStructs "github.com/openshift/managed-upgrade-operator/util/mocks/structs"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	TEST_OPERATOR_NAMESPACE = "openshift-managed-upgrade-operator"
	TEST_UPGRADECONFIG_CR   = "osd-upgrade-config"
	TEST_UPGRADE_VERSION    = "4.4.4"
	TEST_UPGRADE_TIME       = "2020-06-20T00:00:00Z"
)

var _ = Describe("OCM Notifier", func() {
	var (
		mockCtrl                 *gomock.Controller
		mockKubeClient           *mocks.MockClient
		mockUpgradeConfigManager *ucMgrMock.MockUpgradeConfigManager
		mockConfigManagerBuilder *configMock.MockConfigManagerBuilder
		mockNotifier             *notifierMock.MockNotifier
		mockMetricsClient        *metricsMock.MockMetrics
		manager                  *eventManager
		upgradeConfigName        types.NamespacedName
	)

	BeforeEach(func() {
		_ = os.Setenv("OPERATOR_NAMESPACE", TEST_OPERATOR_NAMESPACE)
		mockCtrl = gomock.NewController(GinkgoT())
		mockKubeClient = mocks.NewMockClient(mockCtrl)
		mockUpgradeConfigManager = ucMgrMock.NewMockUpgradeConfigManager(mockCtrl)
		mockConfigManagerBuilder = configMock.NewMockConfigManagerBuilder(mockCtrl)
		mockNotifier = notifierMock.NewMockNotifier(mockCtrl)
		mockMetricsClient = metricsMock.NewMockMetrics(mockCtrl)
	})

	JustBeforeEach(func() {
		manager = &eventManager{
			client:               mockKubeClient,
			upgradeConfigManager: mockUpgradeConfigManager,
			notifier:             mockNotifier,
			metrics:              mockMetricsClient,
			configManagerBuilder: mockConfigManagerBuilder,
		}
	})

	AfterEach(func() {
		mockCtrl.Finish()
	})

	Context("When notifying a state", func() {
		var uc upgradev1alpha1.UpgradeConfig
		var testState = notifier.StateCompleted
		BeforeEach(func() {
			upgradeConfigName = types.NamespacedName{
				Name:      TEST_UPGRADECONFIG_CR,
				Namespace: TEST_OPERATOR_NAMESPACE,
			}
			uc = *testStructs.NewUpgradeConfigBuilder().WithNamespacedName(upgradeConfigName).WithPhase(upgradev1alpha1.UpgradePhaseUpgraded).GetUpgradeConfig()
			uc.Spec.Desired.Version = TEST_UPGRADE_VERSION
			uc.Status.History[0].Version = TEST_UPGRADE_VERSION
			uc.Spec.UpgradeAt = TEST_UPGRADE_TIME
			uc.Status.NotificationEvent.Failed = false
			uc.Status.NotificationEvent.Sent = true
			uc.Status.NotificationEvent.State = string(testState)
		})

		Context("when a notification has already been sent", func() {
			It("does no action", func() {
				gomock.InOrder(
					mockUpgradeConfigManager.EXPECT().Get().Return(&uc, nil),
				)
				err := manager.Notify(testState)
				Expect(err).To(BeNil())
			})
		})
		Context("when a notification has not been sent", func() {
			var notificationNotSentUC upgradev1alpha1.UpgradeConfig
			notificationNotSentUC = uc
			notificationNotSentUC.Status.NotificationEvent.Sent = false
			It("sends a correct notification", func() {
				gomock.InOrder(
					mockUpgradeConfigManager.EXPECT().Get().Return(&notificationNotSentUC, nil),
					mockNotifier.EXPECT().NotifyState(testState, gomock.Any()),
				)
				err := manager.Notify(testState)
				Expect(err).To(BeNil())
			})
		})
		Context("when a notification can't be sent", func() {
			var fakeError = fmt.Errorf("fake error")
			var notificationFailedUC upgradev1alpha1.UpgradeConfig
			notifcationFailedUC := uc
			notifcationFailedUC.Status.NotificationEvent.Failed = true
			notifcationFailedUC.Status.NotificationEvent.State = string(testState)
			It("returns an error", func() {
				gomock.InOrder(
					mockUpgradeConfigManager.EXPECT().Get().Return(&notificationFailedUC, nil),
					mockNotifier.EXPECT().NotifyState(testState, gomock.Any()).Return(fakeError),
				)
				err := manager.Notify(testState)
				Expect(err).NotTo(BeNil())
			})
		})

	})
})
