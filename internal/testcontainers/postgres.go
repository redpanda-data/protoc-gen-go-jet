package testcontainers

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/redpanda-data/protoc-gen-go-jet/internal/tests"
)

// Postgres is a wrapper around testcontainers-go specifically for postgres db
// with the logical-replication settings the integration tests rely on.
type Postgres struct {
	testcontainers.Container
	connectionString     string
	rawConnectionString  string
	jdbcConnectionString string
	psqlConn             string

	tmpFiles []string
}

// NewPostgres creates a new postgres testcontainer
func NewPostgres() (*Postgres, error) {
	return NewPostgresWithConfigs(nil, nil)
}

// NewPostgresWithConfigs creates a new postgres testcontainer with a custom database name.
func NewPostgresWithConfigs(postgresqlConf []byte, pgHbaConf []byte) (*Postgres, error) {
	return NewPostgresWithConfigsAndCustomDB("postgres", postgresqlConf, pgHbaConf)
}

// NewPostgresWithConfigsAndCustomDB creates a new postgres testcontainer with a custom database name.
func NewPostgresWithConfigsAndCustomDB(database string, postgresqlConf []byte, pgHbaConf []byte) (*Postgres, error) {
	req := testcontainers.ContainerRequest{
		Image:        "postgres:13.6",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_DB":       database,
			"POSTGRES_USER":     "postgres",
			"POSTGRES_PASSWORD": "postgres",
		},
		WaitingFor: wait.ForAll(
			// This log is a lie, it is NOT immediately after this log message ready, we also need to probe the port..
			wait.ForLog("database system is ready to accept connections"),
			wait.NewHostPortStrategy("5432")),
	}

	req, tmpFiles, err := customizePostgresConfig(postgresqlConf, pgHbaConf, req)
	if err != nil {
		return nil, err
	}

	pgContainer, err := testcontainers.GenericContainer(context.Background(), testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, err
	}

	pgPort, err := pgContainer.MappedPort(context.Background(), "5432")
	if err != nil {
		return nil, err
	}

	pgHost, err := pgContainer.Host(context.Background())
	if err != nil {
		return nil, err
	}

	pg := &Postgres{
		Container:            pgContainer,
		connectionString:     fmt.Sprintf("postgres://postgres:postgres@%s:%d/postgres?sslmode=disable", pgHost, pgPort.Num()),
		rawConnectionString:  fmt.Sprintf("postgres://postgres:postgres@%s:%d/postgres", pgHost, pgPort.Num()),
		jdbcConnectionString: fmt.Sprintf("jdbc:postgresql://%s:%d/postgres?sslmode=disable", pgHost, pgPort.Num()),
		psqlConn:             fmt.Sprintf("psql -h %s -p %d -U postgres postgres", pgHost, pgPort.Num()),
		tmpFiles:             tmpFiles,
	}

	const waitTimeout = time.Second * 30
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	if err := tests.Wait(ctx, time.Millisecond*100, func() error {
		conn, err := pgx.Connect(ctx, pg.ConnectionString())
		if err != nil {
			return err
		}
		defer conn.Close(ctx)
		return conn.Ping(ctx)
	}); err != nil {
		return nil, fmt.Errorf("failed to wait for pg: %w", err)
	}

	return pg, err
}

// PgConfReplication is the content of the pg_hba.conf file with
// replication enabled.
//
//go:embed postgresql-replication.conf
var PgConfReplication []byte

// PgHbaConfReplication is the content of the postgresql.conf file with
// replication enabled.
//
//go:embed pg_hba-replication.conf
var PgHbaConfReplication []byte

func customizePostgresConfig(postgresqlConf []byte, pgHbaConf []byte,
	req testcontainers.ContainerRequest,
) (testcontainers.ContainerRequest, []string, error) {
	var tmpFiles []string
	if postgresqlConf != nil && pgHbaConf != nil {
		postgresSQLConfFile, err := os.CreateTemp("", "")
		if err != nil {
			return testcontainers.ContainerRequest{}, tmpFiles, err
		}

		tmpFiles = append(tmpFiles, postgresSQLConfFile.Name())

		if err := os.WriteFile(postgresSQLConfFile.Name(), postgresqlConf, 0o700); err != nil { //nolint:gosec //file permissions work fine
			return testcontainers.ContainerRequest{}, nil, err
		}

		pgHbaConfig, err := os.CreateTemp("", "")
		if err != nil {
			return testcontainers.ContainerRequest{}, nil, err
		}

		tmpFiles = append(tmpFiles, pgHbaConfig.Name())

		if err := os.WriteFile(pgHbaConfig.Name(), pgHbaConf, 0o700); err != nil { //nolint:gosec //file permissions work fine
			return testcontainers.ContainerRequest{}, tmpFiles, err
		}

		req.Files = []testcontainers.ContainerFile{
			{
				HostFilePath:      postgresSQLConfFile.Name(),
				ContainerFilePath: "/postgresql.conf",
				FileMode:          444,
			},
			{
				HostFilePath:      pgHbaConfig.Name(),
				ContainerFilePath: "/pg_hba.conf",
				FileMode:          444,
			},
		}
		req.Cmd = []string{
			"postgres",
			"-c", "config_file=/postgresql.conf",
			"-c", "hba_file=/pg_hba.conf",
		}
	}
	return req, tmpFiles, nil
}

// ConnectionString returns the connection string
func (p *Postgres) ConnectionString() string {
	return p.connectionString
}

// RawConnectionString returns the connection string without any options or parameters
func (p *Postgres) RawConnectionString() string {
	return p.rawConnectionString
}

// JdbcConnectionString returns the JDBC connection string
func (p *Postgres) JdbcConnectionString() string {
	return p.jdbcConnectionString
}

// PsqlConnectionCommand returns psql command to connect to the database
func (p *Postgres) PsqlConnectionCommand() string {
	return p.psqlConn
}

// Terminate the the container, calling the underlying testcontainer's
// Terminate, and cleaning up temporary files
func (p *Postgres) Terminate(ctx context.Context) error {
	for _, file := range p.tmpFiles {
		_ = os.Remove(file)
	}
	return p.Container.Terminate(ctx)
}
