// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	dms "github.com/aws/aws-sdk-go-v2/service/databasemigrationservice"
	dmstypes "github.com/aws/aws-sdk-go-v2/service/databasemigrationservice/types"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/roachprod/logger"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/errors"
	"github.com/jackc/pgx/v4"
	"google.golang.org/protobuf/proto"
)

const (
	awsdmsRoachtestRDSClusterName = "roachtest-awsdms-rds-cluster"

	awsdmsRoachtestDMSParameterGroup          = "roachtest-awsdms-param-group"
	awsdmsRoachtestDMSTaskName                = "roachtest-awsdms-dms-task"
	awsdmsRoachtestDMSReplicationInstanceName = "roachtest-awsdms-replication-instance"
	awsdmsRoachtestDMSRDSEndpointName         = "roachtest-awsdms-rds-endpoint"
	awsdmsRoachtestDMSCRDBEndpointName        = "roachtest-awsdms-crdb-endpoint"

	awsdmsWaitTimeLimit  = 30 * time.Minute
	awsdmsUser           = "cockroachdbtest"
	awsdmsDatabase       = "rdsdb"
	awsdmsCRDBDatabase   = "defaultdb"
	awsdmsCRDBUser       = "dms"
	awsdmsNumInitialRows = 100000
)

var (
	rdsClusterFilters = []rdstypes.Filter{
		{
			Name:   proto.String("db-cluster-id"),
			Values: []string{awsdmsRoachtestRDSClusterName},
		},
	}
	rdsDescribeInstancesInput = &rds.DescribeDBInstancesInput{
		Filters: rdsClusterFilters,
	}
	dmsDescribeInstancesInput = &dms.DescribeReplicationInstancesInput{
		Filters: []dmstypes.Filter{
			{
				Name:   proto.String("replication-instance-id"),
				Values: []string{awsdmsRoachtestDMSReplicationInstanceName},
			},
		},
	}
	dmsDescribeTasksInput = &dms.DescribeReplicationTasksInput{
		Filters: []dmstypes.Filter{
			{
				Name:   proto.String("replication-task-id"),
				Values: []string{awsdmsRoachtestDMSTaskName},
			},
		},
	}
)

func registerAWSDMS(r registry.Registry) {
	r.Add(registry.TestSpec{
		Name:    "awsdms",
		Owner:   registry.OwnerSQLExperience, // TODO(otan): add a migrations OWNERS team
		Cluster: r.MakeClusterSpec(1),
		Tags:    []string{`default`, `awsdms`},
		Run:     runAWSDMS,
	})
}

// runAWSDMS creates Amazon RDS instances to import into CRDB using AWS DMS.
//
// The RDS and DMS instances are always created with the same names, so that
// we can always start afresh with a new instance and that we can assume
// there is only ever one of these at any time. On startup and teardown,
// we will attempt to delete previously created instances.
func runAWSDMS(ctx context.Context, t test.Test, c cluster.Cluster) {
	if c.IsLocal() {
		t.Fatal("cannot be run in local mode")
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithDefaultRegion("us-east-1"))
	if err != nil {
		t.Fatal(err)
	}
	rdsCli := rds.NewFromConfig(awsCfg)
	dmsCli := dms.NewFromConfig(awsCfg)

	// Attempt a clean-up of old instances on startup.
	t.L().Printf("attempting to delete old instances")
	if err := tearDownAWSDMS(ctx, t.L(), rdsCli, dmsCli); err != nil {
		t.Fatal(err)
	}

	// Attempt a clean-up of old instances on shutdown.
	defer func() {
		if t.IsDebug() {
			t.L().Printf("not deleting old instances as --debug is set")
			return
		}
		t.L().Printf("attempting to cleanup instances")
		// Try to delete from a new context, in case the previous one is cancelled.
		if err := tearDownAWSDMS(context.Background(), t.L(), rdsCli, dmsCli); err != nil {
			t.L().Printf("failed to delete old instances on cleanup: %+v", err)
		}
	}()

	sourcePGConn, err := setupAWSDMS(ctx, t, c, rdsCli, dmsCli)
	if err != nil {
		t.Fatal(err)
	}
	targetPGConn := c.Conn(ctx, t.L(), 1)

	waitForReplicationRetryOpts := retry.Options{
		MaxBackoff: time.Second,
		MaxRetries: 90,
	}

	// Unfortunately validation isn't available in the SDK. For now, just assert
	// both tables have the same number of rows.
	t.L().Printf("testing all data gets replicated")
	if err := func() error {
		for r := retry.StartWithCtx(ctx, waitForReplicationRetryOpts); r.Next(); {
			var numRows int
			if err := targetPGConn.QueryRow("SELECT count(1) FROM test_table").Scan(&numRows); err != nil {
				return err
			}
			if numRows == awsdmsNumInitialRows {
				return nil
			}
			t.L().Printf("found %d rows when expecting %d, retrying", numRows, awsdmsNumInitialRows)
		}
		return errors.Newf("failed to find target in sync")
	}(); err != nil {
		t.Fatal(err)
	}

	// Now check an INSERT, UPDATE and DELETE all gets replicated.
	const (
		numExtraRows   = 10
		numDeletedRows = 1
		deleteRowID    = 55
		updateRowID    = 742
		updateRowText  = "from now on the baby sleeps in the crib"
	)

	for _, stmt := range []string{
		fmt.Sprintf(
			`INSERT INTO test_table(id, t) SELECT i, md5(random()::text) FROM generate_series(%d, %d) AS t(i)`,
			awsdmsNumInitialRows+1,
			awsdmsNumInitialRows+numExtraRows,
		),
		fmt.Sprintf(`UPDATE test_table SET t = '%s' WHERE id = %d`, updateRowText, updateRowID),
		fmt.Sprintf(`DELETE FROM test_table WHERE id = %d`, deleteRowID),
	} {
		if _, err := sourcePGConn.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	t.L().Printf("testing all subsequent updates get replicated")
	if err := func() error {
		for r := retry.StartWithCtx(ctx, waitForReplicationRetryOpts); r.Next(); {
			err := func() error {
				var countOfDeletedRow int
				if err := targetPGConn.QueryRow("SELECT count(1) FROM test_table WHERE id = $1", deleteRowID).Scan(&countOfDeletedRow); err != nil {
					return err
				}
				if countOfDeletedRow != 0 {
					return errors.Newf("expected row to be deleted, still found")
				}

				var seenText string
				if err := targetPGConn.QueryRow("SELECT t FROM test_table WHERE id = $1", updateRowID).Scan(&seenText); err != nil {
					return err
				}
				if seenText != updateRowText {
					return errors.Newf("expected row to be updated, still found %s", seenText)
				}

				var numRows int
				if err := targetPGConn.QueryRow("SELECT count(1) FROM test_table").Scan(&numRows); err != nil {
					return err
				}
				expectedRows := awsdmsNumInitialRows + numExtraRows - numDeletedRows
				if numRows != expectedRows {
					return errors.Newf("found %d rows when expecting %d, retrying", numRows, expectedRows)
				}
				return nil
			}()
			if err == nil {
				return nil
			}
			t.L().Printf("replication not up to date, retrying: %+v", err)
		}
		return errors.Newf("failed to find target in sync")
	}(); err != nil {
		t.Fatal(err)
	}

	t.L().Printf("testing complete")
}

// setupAWSDMS sets up an RDS instance and a DMS instance which sets up a
// migration task from the RDS instance to the CockroachDB cluster.
func setupAWSDMS(
	ctx context.Context, t test.Test, c cluster.Cluster, rdsCli *rds.Client, dmsCli *dms.Client,
) (*pgx.Conn, error) {
	var sourcePGConn *pgx.Conn
	if err := func() error {
		var rdsCluster *rdstypes.DBCluster
		var replicationARN string

		var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
		awsdmsPassword := func() string {
			b := make([]rune, 32)
			for i := range b {
				b[i] = letters[rand.Intn(len(letters))]
			}
			return string(b)
		}()

		g := ctxgroup.WithContext(ctx)
		g.Go(setupRDSCluster(ctx, t, rdsCli, awsdmsPassword, &rdsCluster, &sourcePGConn))
		g.Go(setupCockroachDBCluster(ctx, t, c))
		g.Go(setupDMSReplicationInstance(ctx, t, dmsCli, &replicationARN))

		if err := g.Wait(); err != nil {
			return err
		}

		if err := setupDMSEndpointsAndTask(ctx, t, c, dmsCli, rdsCluster, awsdmsPassword, replicationARN); err != nil {
			return err
		}
		return nil
	}(); err != nil {
		return nil, errors.Wrapf(err, "failed to set up AWS DMS")
	}
	return sourcePGConn, nil
}

func setupCockroachDBCluster(ctx context.Context, t test.Test, c cluster.Cluster) func() error {
	return func() error {
		t.L().Printf("setting up cockroach")
		c.Put(ctx, t.Cockroach(), "./cockroach", c.All())
		c.Start(ctx, t.L(), option.DefaultStartOpts(), install.MakeClusterSettings(), c.All())

		db := c.Conn(ctx, t.L(), 1)
		for _, stmt := range []string{
			fmt.Sprintf("CREATE USER %s", awsdmsCRDBUser),
			fmt.Sprintf("GRANT admin TO %s", awsdmsCRDBUser),
			fmt.Sprintf("ALTER USER %s SET expect_and_ignore_not_visible_columns_in_copy = true", awsdmsCRDBUser),
		} {
			if _, err := db.Exec(stmt); err != nil {
				return err
			}
		}
		return nil
	}
}

func setupDMSReplicationInstance(
	ctx context.Context, t test.Test, dmsCli *dms.Client, replicationARN *string,
) func() error {
	return func() error {
		t.L().Printf("setting up DMS replication instance")
		createReplOut, err := dmsCli.CreateReplicationInstance(
			ctx,
			&dms.CreateReplicationInstanceInput{
				ReplicationInstanceClass:      proto.String("dms.c4.large"),
				ReplicationInstanceIdentifier: proto.String(awsdmsRoachtestDMSReplicationInstanceName),
			},
		)
		if err != nil {
			return err
		}
		*replicationARN = *createReplOut.ReplicationInstance.ReplicationInstanceArn
		// Wait for replication instance to become available
		t.L().Printf("waiting for all replication instance to be available")
		if err := dms.NewReplicationInstanceAvailableWaiter(dmsCli).Wait(ctx, dmsDescribeInstancesInput, awsdmsWaitTimeLimit); err != nil {
			return err
		}
		return nil
	}
}

func setupRDSCluster(
	ctx context.Context,
	t test.Test,
	rdsCli *rds.Client,
	awsdmsPassword string,
	rdsCluster **rdstypes.DBCluster,
	sourcePGConn **pgx.Conn,
) func() error {
	return func() error {
		// Setup AWS RDS.
		t.L().Printf("setting up new AWS RDS parameter group")
		rdsGroup, err := rdsCli.CreateDBClusterParameterGroup(
			ctx,
			&rds.CreateDBClusterParameterGroupInput{
				DBParameterGroupFamily:      proto.String("aurora-postgresql13"),
				DBClusterParameterGroupName: proto.String(awsdmsRoachtestDMSParameterGroup),
				Description:                 proto.String("roachtest awsdms parameter groups"),
			},
		)
		if err != nil {
			return err
		}

		if _, err := rdsCli.ModifyDBClusterParameterGroup(
			ctx,
			&rds.ModifyDBClusterParameterGroupInput{
				DBClusterParameterGroupName: rdsGroup.DBClusterParameterGroup.DBClusterParameterGroupName,
				Parameters: []rdstypes.Parameter{
					{
						ParameterName:  proto.String("rds.logical_replication"),
						ParameterValue: proto.String("1"),
						// Using ApplyMethodImmediate will error (it is not accepted for
						// this parameter), so using `ApplyMethodPendingReboot` instead. We
						// haven't started the cluster yet, so we can rely on this being
						// setup on first instantiation of the cluster.
						ApplyMethod: rdstypes.ApplyMethodPendingReboot,
					},
				},
			},
		); err != nil {
			return err
		}

		t.L().Printf("setting up new AWS RDS cluster")
		rdsClusterOutput, err := rdsCli.CreateDBCluster(
			ctx,
			&rds.CreateDBClusterInput{
				DBClusterIdentifier:         proto.String(awsdmsRoachtestRDSClusterName),
				Engine:                      proto.String("aurora-postgresql"),
				DBClusterParameterGroupName: proto.String(awsdmsRoachtestDMSParameterGroup),
				MasterUsername:              proto.String(awsdmsUser),
				MasterUserPassword:          proto.String(awsdmsPassword),
				DatabaseName:                proto.String(awsdmsDatabase),
			},
		)
		if err != nil {
			return err
		}
		*rdsCluster = rdsClusterOutput.DBCluster

		t.L().Printf("setting up new AWS RDS instance")
		if _, err := rdsCli.CreateDBInstance(
			ctx,
			&rds.CreateDBInstanceInput{
				DBInstanceClass:      proto.String("db.r5.large"),
				DBInstanceIdentifier: proto.String(awsdmsRoachtestRDSClusterName + "-1"),
				Engine:               proto.String("aurora-postgresql"),
				DBClusterIdentifier:  proto.String(awsdmsRoachtestRDSClusterName),
				PubliclyAccessible:   proto.Bool(true),
			},
		); err != nil {
			return err
		}

		t.L().Printf("waiting for RDS instances to become available")
		if err := rds.NewDBInstanceAvailableWaiter(rdsCli).Wait(ctx, rdsDescribeInstancesInput, awsdmsWaitTimeLimit); err != nil {
			return err
		}
		pgURL := fmt.Sprintf(
			"postgres://%s:%s@%s:%d/%s",
			awsdmsUser,
			awsdmsPassword,
			*rdsClusterOutput.DBCluster.Endpoint,
			*rdsClusterOutput.DBCluster.Port,
			*rdsClusterOutput.DBCluster.DatabaseName,
		)
		if t.IsDebug() {
			t.L().Printf("pgurl: %s\n", pgURL)
		}
		pgConn, err := pgx.Connect(ctx, pgURL)
		if err != nil {
			return err
		}
		for _, stmt := range []string{
			`CREATE TABLE test_table(id integer PRIMARY KEY, t TEXT)`,
			fmt.Sprintf(
				`INSERT INTO test_table(id, t) SELECT i, md5(random()::text) FROM generate_series(1, %d) AS t(i)`,
				awsdmsNumInitialRows,
			),
		} {
			if _, err := pgConn.Exec(
				ctx,
				stmt,
			); err != nil {
				return err
			}
		}
		*sourcePGConn = pgConn
		return nil
	}
}

func setupDMSEndpointsAndTask(
	ctx context.Context,
	t test.Test,
	c cluster.Cluster,
	dmsCli *dms.Client,
	rdsCluster *rdstypes.DBCluster,
	awsdmsPassword string,
	replicationARN string,
) error {
	// Setup AWS DMS to replicate to CockroachDB.
	externalCRDBAddr, err := c.ExternalIP(ctx, t.L(), option.NodeListOption{1})
	if err != nil {
		return err
	}

	var sourceARN, targetARN string
	for _, ep := range []struct {
		in  dms.CreateEndpointInput
		arn *string
	}{
		{
			in: dms.CreateEndpointInput{
				EndpointIdentifier: proto.String(awsdmsRoachtestDMSRDSEndpointName),
				EndpointType:       dmstypes.ReplicationEndpointTypeValueSource,
				EngineName:         proto.String("aurora-postgresql"),
				DatabaseName:       proto.String(awsdmsDatabase),
				Username:           rdsCluster.MasterUsername,
				Password:           proto.String(awsdmsPassword),
				Port:               rdsCluster.Port,
				ServerName:         rdsCluster.Endpoint,
			},
			arn: &sourceARN,
		},
		{
			in: dms.CreateEndpointInput{
				EndpointIdentifier: proto.String(awsdmsRoachtestDMSCRDBEndpointName),
				EndpointType:       dmstypes.ReplicationEndpointTypeValueTarget,
				EngineName:         proto.String("postgres"),
				SslMode:            dmstypes.DmsSslModeValueNone,
				PostgreSQLSettings: &dmstypes.PostgreSQLSettings{
					DatabaseName: proto.String(awsdmsCRDBDatabase),
					Username:     proto.String(awsdmsCRDBUser),
					// Password is a required field, but CockroachDB doesn't take passwords in
					// --insecure mode. As such, put in some garbage.
					Password:   proto.String("garbage"),
					Port:       proto.Int32(26257),
					ServerName: proto.String(externalCRDBAddr[0]),
				},
			},
			arn: &targetARN,
		},
	} {
		t.L().Printf("creating replication endpoint %s", *ep.in.EndpointIdentifier)
		epOut, err := dmsCli.CreateEndpoint(ctx, &ep.in)
		if err != nil {
			return err
		}
		*ep.arn = *epOut.Endpoint.EndpointArn
	}

	t.L().Printf("creating replication task")
	replTaskOut, err := dmsCli.CreateReplicationTask(
		ctx,
		&dms.CreateReplicationTaskInput{
			MigrationType:             dmstypes.MigrationTypeValueFullLoadAndCdc,
			ReplicationInstanceArn:    proto.String(replicationARN),
			ReplicationTaskIdentifier: proto.String(awsdmsRoachtestDMSTaskName),
			SourceEndpointArn:         proto.String(sourceARN),
			TargetEndpointArn:         proto.String(targetARN),
			// TODO(#migrations): when AWS API supports EnableValidation, add it here.
			TableMappings: proto.String(`{
    "rules": [
        {
            "rule-type": "selection",
            "rule-id": "1",
            "rule-name": "1",
            "object-locator": {
                "schema-name": "%",
                "table-name": "%"
            },
            "rule-action": "include"
        }
    ]
}`),
		},
	)
	if err != nil {
		return err
	}
	t.L().Printf("waiting for replication task to be ready")
	if err := dms.NewReplicationTaskReadyWaiter(dmsCli).Wait(ctx, dmsDescribeTasksInput, awsdmsWaitTimeLimit); err != nil {
		return err
	}
	t.L().Printf("starting replication task")
	if _, err := dmsCli.StartReplicationTask(
		ctx,
		&dms.StartReplicationTaskInput{
			ReplicationTaskArn:       replTaskOut.ReplicationTask.ReplicationTaskArn,
			StartReplicationTaskType: dmstypes.StartReplicationTaskTypeValueReloadTarget,
		},
	); err != nil {
		return err
	}
	t.L().Printf("waiting for replication task to be running")
	if err := dms.NewReplicationTaskRunningWaiter(dmsCli).Wait(ctx, dmsDescribeTasksInput, awsdmsWaitTimeLimit); err != nil {
		return err
	}
	return nil
}

func isDMSResourceNotFound(err error) bool {
	return errors.HasType(err, &dmstypes.ResourceNotFoundFault{})
}

func tearDownAWSDMS(
	ctx context.Context, l *logger.Logger, rdsCli *rds.Client, dmsCli *dms.Client,
) error {
	if err := func() error {
		if err := tearDownDMSTasks(ctx, l, dmsCli); err != nil {
			return err
		}
		if err := tearDownDMSEndpoints(ctx, l, dmsCli); err != nil {
			return err
		}

		// Delete the replication and rds instances in parallel.
		g := ctxgroup.WithContext(ctx)
		g.Go(tearDownDMSInstances(ctx, l, dmsCli))
		g.Go(tearDownRDSInstances(ctx, l, rdsCli))
		return g.Wait()
	}(); err != nil {
		return errors.Wrapf(err, "failed to tear down DMS")
	}
	return nil
}

// tearDownDMSTasks tears down the DMS task, endpoints and replication instance
// that may have been created.
func tearDownDMSTasks(ctx context.Context, l *logger.Logger, dmsCli *dms.Client) error {
	dmsTasks, err := dmsCli.DescribeReplicationTasks(ctx, dmsDescribeTasksInput)
	if err != nil {
		if !isDMSResourceNotFound(err) {
			return err
		}
	} else {
		wasRunning := false
		for _, task := range dmsTasks.ReplicationTasks {
			if *task.Status == "running" {
				l.Printf("stopping DMS task %s (arn: %s)", *task.ReplicationTaskIdentifier, *task.ReplicationTaskArn)
				if _, err := dmsCli.StopReplicationTask(ctx, &dms.StopReplicationTaskInput{ReplicationTaskArn: task.ReplicationTaskArn}); err != nil {
					return err
				}
				wasRunning = true
			}
		}
		if wasRunning {
			l.Printf("waiting for task to be stopped")
			if err := dms.NewReplicationTaskStoppedWaiter(dmsCli).Wait(ctx, dmsDescribeTasksInput, awsdmsWaitTimeLimit); err != nil {
				return err
			}
		}
		for _, task := range dmsTasks.ReplicationTasks {
			l.Printf("deleting DMS task %s (arn: %s)", *task.ReplicationTaskIdentifier, *task.ReplicationTaskArn)
			if _, err := dmsCli.DeleteReplicationTask(ctx, &dms.DeleteReplicationTaskInput{ReplicationTaskArn: task.ReplicationTaskArn}); err != nil {
				return err
			}
		}
		l.Printf("waiting for task to be deleted")
		if err := dms.NewReplicationTaskDeletedWaiter(dmsCli).Wait(ctx, dmsDescribeTasksInput, awsdmsWaitTimeLimit); err != nil {
			return err
		}
	}
	return nil
}

func tearDownDMSEndpoints(ctx context.Context, l *logger.Logger, dmsCli *dms.Client) error {
	for _, ep := range []string{awsdmsRoachtestDMSRDSEndpointName, awsdmsRoachtestDMSCRDBEndpointName} {
		dmsEndpoints, err := dmsCli.DescribeEndpoints(ctx, &dms.DescribeEndpointsInput{
			Filters: []dmstypes.Filter{
				{
					Name:   proto.String("endpoint-id"),
					Values: []string{ep},
				},
			},
		})
		if err != nil {
			if !isDMSResourceNotFound(err) {
				return err
			}
		} else {
			for _, dmsEndpoint := range dmsEndpoints.Endpoints {
				l.Printf("deleting DMS endpoint %s (arn: %s)", *dmsEndpoint.EndpointIdentifier, *dmsEndpoint.EndpointArn)
				if _, err := dmsCli.DeleteEndpoint(ctx, &dms.DeleteEndpointInput{EndpointArn: dmsEndpoint.EndpointArn}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func tearDownDMSInstances(ctx context.Context, l *logger.Logger, dmsCli *dms.Client) func() error {
	return func() error {
		dmsInstances, err := dmsCli.DescribeReplicationInstances(ctx, dmsDescribeInstancesInput)
		if err != nil {
			if !isDMSResourceNotFound(err) {
				return err
			}
		} else {
			for _, dmsInstance := range dmsInstances.ReplicationInstances {
				l.Printf("deleting DMS replication instance %s (arn: %s)", *dmsInstance.ReplicationInstanceIdentifier, *dmsInstance.ReplicationInstanceArn)
				if _, err := dmsCli.DeleteReplicationInstance(ctx, &dms.DeleteReplicationInstanceInput{
					ReplicationInstanceArn: dmsInstance.ReplicationInstanceArn,
				}); err != nil {
					return err
				}
			}

			// Wait for the replication instance to be deleted.
			l.Printf("waiting for all replication instances to be deleted")
			if err := dms.NewReplicationInstanceDeletedWaiter(dmsCli).Wait(ctx, dmsDescribeInstancesInput, awsdmsWaitTimeLimit); err != nil {
				return err
			}
		}
		return nil
	}
}

func tearDownRDSInstances(ctx context.Context, l *logger.Logger, rdsCli *rds.Client) func() error {
	return func() error {
		rdsInstances, err := rdsCli.DescribeDBInstances(ctx, rdsDescribeInstancesInput)
		if err != nil {
			if !errors.HasType(err, &rdstypes.ResourceNotFoundFault{}) {
				return err
			}
		} else {
			for _, rdsInstance := range rdsInstances.DBInstances {
				l.Printf("attempting to delete instance %s", *rdsInstance.DBInstanceIdentifier)
				if _, err := rdsCli.DeleteDBInstance(
					ctx,
					&rds.DeleteDBInstanceInput{
						DBInstanceIdentifier:   rdsInstance.DBInstanceIdentifier,
						DeleteAutomatedBackups: proto.Bool(true),
						SkipFinalSnapshot:      true,
					},
				); err != nil {
					return err
				}
			}
			l.Printf("waiting for all cluster db instances to be deleted")
			if err := rds.NewDBInstanceDeletedWaiter(rdsCli).Wait(ctx, rdsDescribeInstancesInput, awsdmsWaitTimeLimit); err != nil {
				return err
			}
		}

		// Delete RDS clusters that may be created.
		rdsClusters, err := rdsCli.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
			Filters: rdsClusterFilters,
		})
		if err != nil {
			if !errors.HasType(err, &rdstypes.ResourceNotFoundFault{}) {
				return err
			}
		} else {
			for _, rdsCluster := range rdsClusters.DBClusters {
				l.Printf("attempting to delete cluster %s", *rdsCluster.DBClusterIdentifier)
				if _, err := rdsCli.DeleteDBCluster(
					ctx,
					&rds.DeleteDBClusterInput{
						DBClusterIdentifier: rdsCluster.DBClusterIdentifier,
						SkipFinalSnapshot:   true,
					},
				); err != nil {
					return err
				}
			}
		}
		rdsParamGroups, err := rdsCli.DescribeDBClusterParameterGroups(ctx, &rds.DescribeDBClusterParameterGroupsInput{
			DBClusterParameterGroupName: proto.String(awsdmsRoachtestDMSParameterGroup),
		})
		if err != nil {
			// Sometimes they don't deserialize to DBClusterParameterGroupNotFoundFault :\.
			if !errors.HasType(err, &rdstypes.DBClusterParameterGroupNotFoundFault{}) && !errors.HasType(err, &rdstypes.DBParameterGroupNotFoundFault{}) {
				return err
			}
		} else {
			for _, rdsGroup := range rdsParamGroups.DBClusterParameterGroups {
				l.Printf("attempting to delete param group %s", *rdsGroup.DBClusterParameterGroupName)

				// This can sometimes fail as the cluster still relies on the param
				// group but the cluster takes a while to wind down. Ideally, we wait
				// for the cluster to get deleted. However, there is no provided
				// NewDBClusterDeletedWaiter, so we just have to retry it by hand.
				r := retry.StartWithCtx(ctx, retry.Options{
					InitialBackoff: 30 * time.Second,
					MaxBackoff:     time.Minute,
					MaxRetries:     60,
				})
				var lastErr error
				for r.Next() {
					_, err = rdsCli.DeleteDBClusterParameterGroup(
						ctx,
						&rds.DeleteDBClusterParameterGroupInput{
							DBClusterParameterGroupName: rdsGroup.DBClusterParameterGroupName,
						},
					)
					lastErr = err
					if err == nil {
						break
					}
					l.Printf("expected error: failed to delete cluster param group, retrying: %+v", err)
				}
				if lastErr != nil {
					return errors.Wrapf(lastErr, "failed to delete param group")
				}
			}
		}

		return nil
	}
}
