package deprovisioning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kyma-project/control-plane/components/provisioner/internal/apperrors"

	"k8s.io/apimachinery/pkg/runtime/schema"

	gardener_types "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	directorMocks "github.com/kyma-project/control-plane/components/provisioner/internal/director/mocks"
	installationMocks "github.com/kyma-project/control-plane/components/provisioner/internal/installation/mocks"
	"github.com/kyma-project/control-plane/components/provisioner/internal/model"
	"github.com/kyma-project/control-plane/components/provisioner/internal/operations"
	gardener_mocks "github.com/kyma-project/control-plane/components/provisioner/internal/operations/stages/deprovisioning/mocks"
	"github.com/kyma-project/control-plane/components/provisioner/internal/persistence/dberrors"
	dbMocks "github.com/kyma-project/control-plane/components/provisioner/internal/provisioning/persistence/dbsession/mocks"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestWaitForClusterDeletion_Run(t *testing.T) {

	type mockFunc func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient)

	cluster := model.Cluster{
		ID: "runtimeID",
		ClusterConfig: model.GardenerConfig{
			Name: clusterName,
		},
		Tenant: "tenant",
	}

	for _, testCase := range []struct {
		description   string
		mockFunc      mockFunc
		expectedStage model.OperationStage
		expectedDelay time.Duration
	}{
		{
			description: "should go to the next step when Shoot was deleted successfully and Runtime exists",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(true, nil)
				directorClient.On("DeleteRuntime", runtimeID, tenant).Return(nil)
				dbSession.On("Commit").Return(nil)
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			expectedStage: nextStageName,
			expectedDelay: 0,
		},
		{
			description: "should go to the next step when Shoot was deleted successfully and Runtime not exists",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(false, nil)
				dbSession.On("Commit").Return(nil)
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			expectedStage: nextStageName,
			expectedDelay: 0,
		},
		{
			description: "should retry on error on RuntimeExists call and go to the next step when Shoot was deleted successfully",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				directorClient.On("RuntimeExists", runtimeID, tenant).Once().Return(false, apperrors.Internal("exists err"))
				directorClient.On("RuntimeExists", runtimeID, tenant).Once().Return(true, nil)
				directorClient.On("DeleteRuntime", runtimeID, tenant).Return(nil)
				dbSession.On("Commit").Return(nil)
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			expectedStage: nextStageName,
			expectedDelay: 0,
		},
		{
			description: "should retry on error on DeleteRuntime call and go to the next step when Shoot was deleted successfully",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(true, nil)
				directorClient.On("DeleteRuntime", runtimeID, tenant).Once().Return(apperrors.Internal("delete error"))
				directorClient.On("DeleteRuntime", runtimeID, tenant).Once().Return(nil)
				dbSession.On("Commit").Return(nil)
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			expectedStage: nextStageName,
			expectedDelay: 0,
		},
		{
			description: "should continue waiting if shoot not deleted",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(&gardener_types.Shoot{}, nil)
			},
			expectedStage: model.WaitForClusterDeletion,
			expectedDelay: 20 * time.Second,
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			// given
			gardenerClient := &gardener_mocks.GardenerClient{}
			dbSessionFactory := &dbMocks.Factory{}
			directorClient := &directorMocks.DirectorClient{}

			testCase.mockFunc(gardenerClient, dbSessionFactory, directorClient)

			waitForClusterDeletionStep := NewWaitForClusterDeletionStep(gardenerClient, dbSessionFactory, directorClient, nextStageName, 10*time.Minute)

			// when
			result, err := waitForClusterDeletionStep.Run(cluster, model.Operation{}, logrus.New())

			// then
			require.NoError(t, err)
			assert.Equal(t, testCase.expectedStage, result.Stage)
			assert.Equal(t, testCase.expectedDelay, result.Delay)
			gardenerClient.AssertExpectations(t)
			directorClient.AssertExpectations(t)
		})
	}

	for _, testCase := range []struct {
		description        string
		mockFunc           mockFunc
		cluster            model.Cluster
		unrecoverableError bool
		errComponent       apperrors.ErrComponent
		errReason          apperrors.ErrReason
		errMsg             string
	}{
		{
			description: "should return error when failed to get shoot",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, errors.New("some error"))
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrGardenerClient,
			errReason:          "",
			errMsg:             "some error",
		},
		{
			description: "should return error when failed to start database transaction",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSessionFactory.On("NewSessionWithinTransaction").Return(nil, dberrors.Internal("some error"))
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrDB,
			errReason:          dberrors.ErrDBInternal,
			errMsg:             "error starting db session with transaction: some error",
		},
		{
			description: "should return error when failed to mark cluster as deleted",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(dberrors.NotFound("some error"))
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrDB,
			errReason:          dberrors.ErrDBNotFound,
			errMsg:             "error marking cluster for deletion: some error",
		},
		{
			description: "should return error when failed to check if Runtime exists",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				dbSession.On("RollbackUnlessCommitted").Return()
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(false, apperrors.External("some error").SetComponent(apperrors.ErrMpsOAuth2))
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrMpsOAuth2,
			errReason:          "",
			errMsg:             "error checking Runtime exists in Director: some error",
		},
		{
			description: "should return error when failed to delete Runtime",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", runtimeID).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				dbSession.On("RollbackUnlessCommitted").Return()
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(true, nil)
				directorClient.On("DeleteRuntime", runtimeID, tenant).Return(apperrors.BadGateway("some error").SetComponent(apperrors.ErrCompassDirector).SetReason(apperrors.ErrReason("Invalid data")).Append("Failed to request"))
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrCompassDirector,
			errReason:          apperrors.ErrReason("Invalid data"),
			errMsg:             "error deleting Runtime form Director: Failed to request, some error",
		},
		{
			description: "should return error when failed to commit database transaction",
			mockFunc: func(gardenerClient *gardener_mocks.GardenerClient, dbSessionFactory *dbMocks.Factory, directorClient *directorMocks.DirectorClient) {
				gardenerClient.On("Get", context.Background(), clusterName, mock.Anything).Return(nil, k8serrors.NewNotFound(schema.GroupResource{}, ""))
				dbSession := &dbMocks.WriteSessionWithinTransaction{}
				dbSession.On("MarkClusterAsDeleted", mock.AnythingOfType("string")).Return(nil)
				dbSessionFactory.On("NewSessionWithinTransaction").Return(dbSession, nil)
				directorClient.On("RuntimeExists", runtimeID, tenant).Return(true, nil)
				directorClient.On("DeleteRuntime", runtimeID, tenant).Return(nil)
				dbSession.On("Commit").Return(dberrors.Internal("some error"))
				dbSession.On("RollbackUnlessCommitted").Return()
			},
			cluster:            cluster,
			unrecoverableError: false,
			errComponent:       apperrors.ErrDB,
			errReason:          dberrors.ErrDBInternal,
			errMsg:             "error commiting transaction: some error",
		},
	} {
		t.Run(testCase.description, func(t *testing.T) {
			// given
			installationSvc := &installationMocks.Service{}
			gardenerClient := &gardener_mocks.GardenerClient{}
			dbSessionFactory := &dbMocks.Factory{}
			directorClient := &directorMocks.DirectorClient{}

			testCase.mockFunc(gardenerClient, dbSessionFactory, directorClient)

			waitForClusterDeletionStep := NewWaitForClusterDeletionStep(gardenerClient, dbSessionFactory, directorClient, nextStageName, 10*time.Minute)

			// when
			_, err := waitForClusterDeletionStep.Run(testCase.cluster, model.Operation{}, logrus.New())
			appErr := operations.ConvertToAppError(err)

			// then
			require.Error(t, err)
			nonRecoverable := operations.NonRecoverableError{}
			require.Equal(t, testCase.unrecoverableError, errors.As(err, &nonRecoverable))
			assert.Equal(t, testCase.errComponent, appErr.Component())
			assert.Equal(t, testCase.errReason, appErr.Reason())
			assert.Equal(t, testCase.errMsg, err.Error())
			installationSvc.AssertExpectations(t)
			gardenerClient.AssertExpectations(t)
			dbSessionFactory.AssertExpectations(t)
			directorClient.AssertExpectations(t)
		})
	}
}
