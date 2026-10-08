package productionops

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/ingressnodes"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/controlplane/xds"
	"ebof-wg-mesh/internal/deploy"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/recovery"
)

func (r *Runner) credentials(ctx context.Context, verify bool) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	keys, signing, err := r.keys(ctx, db)
	if err != nil {
		return err
	}
	defer keys.Close()
	if !verify {
		if err := r.exportSigning(ctx, signing); err != nil {
			return err
		}
	}
	ca, err := signing.PublicBundle(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return err
	}
	var paused bool
	var installation, generation string
	if err := db.QueryRowContext(ctx, `SELECT installation,generation,paused FROM recovery_runtime_authority WHERE singleton=TRUE`).Scan(&installation, &generation, &paused); err != nil {
		return err
	}
	if installation != r.Plan.Installation.ID || generation != r.Plan.Generation {
		return fmt.Errorf("credentials differ from host-admitted generation")
	}
	c, s, err := recoveryConfig(r.Config.RecoveryConfig)
	if err != nil {
		return err
	}
	clear(s.RecoveryKey)
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.Database {
			continue
		}
		if r.Plan.Automatic && (pl.Role == deploy.Builder || pl.Role == deploy.Agent) {
			continue
		}
		files := map[string][]byte{"ca.crt": ca}
		a := reconciliation.Authority{InstallationID: installation, Generation: generation, ClusterID: clusterID(ca), Paused: paused, Checkpoints: paused && r.RecoveryProgress != nil && r.RecoveryProgress.Report != nil && r.RecoveryProgress.ApprovedDigest == r.RecoveryProgress.Report.ApprovalDigest()}
		files["authority.json"], _ = json.Marshal(a)
		env := map[string]string{}
		if prefix, ok := map[deploy.Role]string{deploy.Agent: "AGENT", deploy.Builder: "BUILDER", deploy.ControlPlane: "CONTROLPLANE"}[pl.Role]; ok {
			listener := map[deploy.Role]string{deploy.Agent: "127.0.0.1:9091", deploy.Builder: "127.0.0.1:9092", deploy.ControlPlane: "127.0.0.1:9090"}[pl.Role]
			if configured := r.Plan.Installation.Components[pl.Role].Env[prefix+"_HEALTH_LISTEN"]; configured != "" {
				listener = configured
			}
			env[prefix+"_HEALTH_LISTEN"] = listener
		}
		class, client := map[deploy.Role]identity.CallerClass{deploy.Agent: identity.CallerAgent, deploy.Console: identity.CallerDashboard, deploy.Builder: identity.CallerBuilder, deploy.Envoy: identity.CallerIngress}[pl.Role]
		if client {
			material, err := r.clientIdentity(ctx, pl, class, signing, verify)
			if err != nil {
				return err
			}
			files["client.crt"], files["client.key"] = material.CertPEM, material.KeyPEM
			if pl.Role == deploy.Agent {
				if !verify {
					serial, err := identity.CertificateSerialFromPEM(string(material.CertPEM))
					if err != nil {
						return err
					}
					if _, err := db.ExecContext(ctx, `INSERT INTO agent_certificates(serial,agent_id,issued_at) VALUES ($1,$2,statement_timestamp()) ON CONFLICT DO NOTHING`, serial, pl.Instance); err != nil {
						return err
					}
				}
				files["tls/generation"] = []byte(generation)
				files["tls/ca.crt"], files["tls/client.crt"], files["tls/client.key"] = material.CAPEM, material.CertPEM, material.KeyPEM
				env["AGENT_DATA_DIR"] = dataDir(r.Plan, pl)
				env["AGENT_CONTROLPLANE_ADDRESSES"] = r.controlPlaneAddresses("9443")
				env["AGENT_CA_FILE"] = cfgDir(r.Plan, pl) + "/ca.crt"
				env["AGENT_SERVER_NAME"] = r.Config.InternalServerName
			}
			if pl.Role == deploy.Envoy {
				if !verify {
					if err := ingressnodes.New(db).Register(ctx, pl.Instance); err != nil {
						return err
					}
				} else {
					active, err := ingressnodes.New(db).NodeActive(ctx, pl.Instance)
					if err != nil || !active {
						return fmt.Errorf("ingress identity is not active")
					}
				}
				identityDir := cfgDir(r.Plan, pl) + "/ingress"
				local := filepath.Join(r.Config.StateDirectory, r.Plan.Generation, pl.Instance+"-ingress")
				current, readErr := os.ReadFile(filepath.Join(local, "current", "client.crt"))
				if readErr != nil || string(current) != string(material.CertPEM) {
					if verify {
						return fmt.Errorf("ingress identity has not been provisioned")
					}
					if err := xds.WriteIdentity(local, identityDir, material); err != nil {
						return err
					}
				}
				link, err := os.Readlink(filepath.Join(local, "current"))
				if err != nil {
					return err
				}
				err = filepath.WalkDir(local, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
						return nil
					}
					rel, err := filepath.Rel(local, path)
					if err != nil {
						return err
					}
					b, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					files["ingress/"+rel] = b
					return nil
				})
				if err != nil {
					return err
				}
				if verify {
					if _, err := r.remote(ctx, r.Plan, pl, "test \"$(readlink "+shell(identityDir+"/current")+")\" = "+shell(link)+"\n"); err != nil {
						return err
					}
				} else {
					// Switch after all generation files have been uploaded below.
					files[".ingress-current-target"] = []byte(link)
				}
				bootstrap, err := xds.RenderBootstrap(xds.BootstrapConfig{NodeID: pl.Instance, IdentityDir: identityDir, ServerName: r.Config.InternalServerName, XDSAddresses: strings.Split(r.controlPlaneAddresses("18000"), ","), AdminAddress: "127.0.0.1:19000"})
				if err != nil {
					return err
				}
				files["envoy.yaml"] = []byte(bootstrap)
			}
			if pl.Role == deploy.Builder {
				cfg, err := r.builderConfiguration(pl)
				if err != nil {
					return err
				}
				env["BUILDER_EXECUTOR"] = "hardened"
				env["BUILDER_SANDBOX_IMAGE"] = cfg.Sandbox.Image
				env["BUILDER_SANDBOX_SOCKET"] = cfg.Sandbox.Socket
				env["BUILDER_SANDBOX_NAMESPACE"] = cfg.Sandbox.Namespace
				env["BUILDER_SANDBOX_RUNTIME"] = cfg.Sandbox.Runtime
				env["BUILDER_SANDBOX_SNAPSHOTTER"] = cfg.Sandbox.Snapshotter
				env["BUILDER_SANDBOX_CNI_PLUGIN_DIR"] = cfg.Sandbox.CNIPluginDir
				env["BUILDER_SANDBOX_CNI_CONF_DIR"] = cfg.Sandbox.CNIConfDir
				env["BUILDER_SANDBOX_CNI_NETWORK"] = cfg.Sandbox.CNINetwork
				env["BUILDER_SANDBOX_NAMESERVERS"] = strings.Join(cfg.Sandbox.Nameservers, ",")
				env["BUILDER_SANDBOX_BUILDKITD_BINARY"] = cfg.Sandbox.BuildkitdBinary
				env["BUILDER_RAILPACK_FRONTEND_IMAGE"] = cfg.RailpackFrontendImage
				env["BUILDER_RAILPACK_FRONTEND_DIRECTORY"] = cfg.RailpackFrontendDirectory
				env["BUILDER_ID"] = pl.Instance
				env["BUILDER_WORK_DIR"] = dataDir(r.Plan, pl)
				env["BUILDER_CONTROLPLANE_ADDRESSES"] = r.controlPlaneAddresses("9443")
				env["BUILDER_CA_FILE"] = cfgDir(r.Plan, pl) + "/ca.crt"
				env["BUILDER_CERT_FILE"] = cfgDir(r.Plan, pl) + "/client.crt"
				env["BUILDER_KEY_FILE"] = cfgDir(r.Plan, pl) + "/client.key"
				env["BUILDER_SERVER_NAME"] = r.Config.InternalServerName
			}
		}
		if pl.Role == deploy.ControlPlane || pl.Role == deploy.Console {
			files["keyring.json"], err = os.ReadFile(c.KeyringFile)
			if err != nil {
				return err
			}
			if err := r.databaseClient(ctx, pl, db, files, env, verify); err != nil {
				return err
			}
			if pl.Role == deploy.ControlPlane {
				if r.Config.SourceConfig != "" {
					selection, err := r.sourceSelection()
					if err != nil {
						return err
					}
					env["CONTROLPLANE_SOURCE_ARCHIVES_PROVIDER"] = selection.Provider
					env["CONTROLPLANE_SOURCE_ARCHIVES_DIR"] = selection.Directory
					env["CONTROLPLANE_SOURCE_ARCHIVES_S3_ENDPOINT"] = selection.S3.Endpoint
					env["CONTROLPLANE_SOURCE_ARCHIVES_S3_REGION"] = selection.S3.Region
					env["CONTROLPLANE_SOURCE_ARCHIVES_S3_BUCKET"] = selection.S3.Bucket
					env["CONTROLPLANE_SOURCE_ARCHIVES_S3_PREFIX"] = selection.S3.Prefix
					if selection.S3.CredentialsFile != "" {
						b, err := os.ReadFile(selection.S3.CredentialsFile)
						if err != nil {
							return err
						}
						files["source-credentials"] = b
						env["CONTROLPLANE_SOURCE_ARCHIVES_S3_CREDENTIALS_FILE"] = cfgDir(r.Plan, pl) + "/source-credentials"
					}
				}
				// The native deletion gate protects active archives/images using its own
				// selected storage credentials; it never requires the operator's paths.
				gate := recovery.Config{Storage: c.Storage, Images: c.Images, RecoveryKeyFile: cfgDir(r.Plan, pl) + "/recovery.key", KeyringFile: cfgDir(r.Plan, pl) + "/keyring.json"}
				gate.Images.RegistryService = r.Config.RegistryService
				gate.Images.Binary = "/opt/ebpf-wg-mesh/" + r.Plan.Installation.ID + "/" + r.Plan.Release.ID + "/tools/skopeo"
				gate.Images.AuthFile = cfgDir(r.Plan, pl) + "/external-registry-auth.json"
				files["external-registry-auth.json"], err = os.ReadFile(c.Images.AuthFile)
				if err != nil {
					return err
				}
				if c.Images.CertificateDirectory != "" {
					gate.Images.CertificateDirectory = cfgDir(r.Plan, pl) + "/external-registry-tls"
					if err := filepath.WalkDir(c.Images.CertificateDirectory, func(path string, d os.DirEntry, err error) error {
						if err != nil || d.IsDir() {
							return err
						}
						if !d.Type().IsRegular() {
							return fmt.Errorf("registry TLS closure contains a nonregular file")
						}
						rel, err := filepath.Rel(c.Images.CertificateDirectory, path)
						if err != nil {
							return err
						}
						files["external-registry-tls/"+rel], err = os.ReadFile(path)
						return err
					}); err != nil {
						return err
					}
				}
				gate.Storage.CredentialsFile = cfgDir(r.Plan, pl) + "/recovery-storage-credentials"
				for source, target := range map[string]string{c.RecoveryKeyFile: "recovery.key", c.Storage.CredentialsFile: "recovery-storage-credentials"} {
					files[target], err = os.ReadFile(source)
					if err != nil {
						return err
					}
				}
				if c.Storage.CAFile != "" {
					files["recovery-storage-ca.crt"], err = os.ReadFile(c.Storage.CAFile)
					if err != nil {
						return err
					}
					gate.Storage.CAFile = cfgDir(r.Plan, pl) + "/recovery-storage-ca.crt"
				}
				files["recovery.json"], err = json.Marshal(gate)
				if err != nil {
					return err
				}
				env["PLATFORM_RECOVERY_CONFIG"] = cfgDir(r.Plan, pl) + "/recovery.json"
				env["CONTROLPLANE_REGISTRY_HOST"] = r.Config.RegistryService
				env["CONTROLPLANE_REGISTRY_TOKEN_SERVICE"] = r.Config.RegistryService
				env["CONTROLPLANE_REGISTRY_AUTH_LISTEN"] = "0.0.0.0:9444"
				env["CONTROLPLANE_DB_URL"] = string(files["database-url"])
				env["CONTROLPLANE_STATE_DIR"] = dataDir(r.Plan, pl)
				env["CONTROLPLANE_SECRET_KEYS_KEYRING"] = cfgDir(r.Plan, pl) + "/keyring.json"
				var consoles []string
				for _, peer := range r.Plan.Placements {
					if peer.Role == deploy.Console {
						consoles = append(consoles, peer.Instance)
					}
				}
				env["CONTROLPLANE_CONSOLE_CALLER_IDS"] = strings.Join(consoles, ",")
				env["CONTROLPLANE_REPLICA_ADDRESSES"] = r.controlPlaneAddresses("9443")
				h, _ := r.Plan.Installation.Host(pl.Host)
				env["CONTROLPLANE_ADVERTISE_ADDR"] = net.JoinHostPort(h.Network.Address, "9443")
				env["CONTROLPLANE_INTERNAL_SERVER_NAMES"] = r.Config.InternalServerName + "," + h.Network.Address
				env["CONTROLPLANE_INGRESS_XDS_LISTEN"] = "0.0.0.0:18000"
				files["wildcard.crt"], err = os.ReadFile(r.Config.WildcardCertificate)
				if err != nil {
					return err
				}
				files["wildcard.key"], err = os.ReadFile(r.Config.WildcardKey)
				if err != nil {
					return err
				}
				if err := verifyWildcard(files["wildcard.crt"], files["wildcard.key"], r.Config.PlatformDomain); err != nil {
					return err
				}
				env["CONTROLPLANE_INGRESS_PLATFORM_TLS_CERT_FILE"] = cfgDir(r.Plan, pl) + "/wildcard.crt"
				env["CONTROLPLANE_INGRESS_PLATFORM_TLS_KEY_FILE"] = cfgDir(r.Plan, pl) + "/wildcard.key"
				env["CONTROLPLANE_INGRESS_PUBLIC_ADDR"] = r.Config.PlatformDomain
			} else {
				for key, name := range map[string]string{"DASHBOARD_JWT_SECRET": signkeys.ScopeDashboardSession, "DASHBOARD_CONTROLPLANE_USER_ASSERTION_SECRET": signkeys.ScopeUserAssertion} {
					b, err := signing.ActiveSecret(ctx, name)
					if err != nil {
						return err
					}
					env[key] = string(b)
				}
				b, err := os.ReadFile(r.Config.Console.TokenKeyFile)
				if err != nil {
					return err
				}
				env["DASHBOARD_GITHUB_TOKEN_ENCRYPTION_KEY"] = strings.TrimSpace(string(b))
				env["DASHBOARD_JWT_SECRET_PREVIOUS"] = ""
				for _, endpoint := range r.Plan.Installation.Endpoints {
					if endpoint.Role == deploy.Console {
						env["DASHBOARD_PUBLIC_BASE_URL"] = endpoint.URL
						hostURL, err := url.Parse(endpoint.URL)
						if err != nil {
							return err
						}
						env["DASHBOARD_INGRESS_TARGET_HOST"] = hostURL.Hostname()
					}
				}
				parsed, err := url.Parse(string(files["database-url"]))
				if err != nil {
					return err
				}
				selectDatabaseHost(parsed, parsed.Host)
				var urls []string
				for _, address := range strings.Split(r.databaseAddresses(), ",") {
					parsed.Host = address
					urls = append(urls, parsed.String())
				}
				env["DASHBOARD_DATABASE_URL"] = urls[0]
				data, _ := json.Marshal(urls)
				env["DASHBOARD_DATABASE_URLS"] = string(data)
				env["DASHBOARD_DATABASE_SCHEMA"] = r.Config.Console.Schema
				env["DASHBOARD_CONTROLPLANE_ADDRESSES"] = r.controlPlaneAddresses("9443")
				env["DASHBOARD_CONTROLPLANE_SERVER_NAME"] = r.Config.InternalServerName
				env["DASHBOARD_CONTROLPLANE_CA_FILE"] = cfgDir(r.Plan, pl) + "/ca.crt"
				env["DASHBOARD_CONTROLPLANE_CERT_FILE"] = cfgDir(r.Plan, pl) + "/client.crt"
				env["DASHBOARD_CONTROLPLANE_KEY_FILE"] = cfgDir(r.Plan, pl) + "/client.key"
			}
		}
		if pl.Role == deploy.Registry {
			trust, err := signing.PublicBundle(ctx, signkeys.ScopeRegistry)
			if err != nil {
				return err
			}
			files["registry-trust.crt"] = trust
			material, err := signing.Active(ctx, signkeys.ScopeRegistry)
			if err != nil {
				return err
			}
			// One upload authority across all registry replicas, derived from the
			// encrypted signing material already in the complete recovery closure.
			// A new recovery generation invalidates interrupted old uploads.
			mac := hmac.New(sha256.New, material.Private)
			mac.Write([]byte("ebpf-wg-mesh/registry-upload/v1/" + installation + "/" + generation))
			clear(material.Private)
			b, err := r.registryConfiguration(pl, hex.EncodeToString(mac.Sum(nil)))
			if err != nil {
				return err
			}
			files["registry.yaml"] = b
		}
		probe, ok := r.Config.Probes[pl.Role]
		if !ok {
			return fmt.Errorf("missing readiness probe for %s", pl.Role)
		}
		probe = r.expandProbe(probe, pl)
		files["probe.json"], _ = json.Marshal(probe)
		files["runtime.env"] = environment(env)
		var script string
		for _, name := range sortedFiles(files) {
			if name == ".ingress-current-target" {
				continue
			}
			path := cfgDir(r.Plan, pl) + "/" + name
			if strings.HasPrefix(name, "tls/") {
				path = dataDir(r.Plan, pl) + "/" + name
			}
			if verify {
				if pl.Role == deploy.Agent && (name == "tls/client.crt" || name == "tls/client.key") {
					continue
				}
				script += verifyRemoteFile(path, files[name])
			} else {
				script += remoteFile(path, files[name])
			}
		}
		if target := files[".ingress-current-target"]; len(target) > 0 {
			dir := cfgDir(r.Plan, pl) + "/ingress"
			script += "ln -sfn " + shell(string(target)) + " " + shell(dir+"/current.next") + "\nmv -Tf " + shell(dir+"/current.next") + " " + shell(dir+"/current") + "\n"
		}
		if verify && (pl.Role == deploy.ControlPlane || pl.Role == deploy.Console) {
			script += shell(operationsBinary(r.Plan)) + " sql-client " + shell(cfgDir(r.Plan, pl)+"/database-url") + " " + shell(r.Config.Console.Schema) + "\n"
		}
		if _, err := r.remote(ctx, r.Plan, pl, script); err != nil {
			return err
		}
		if pl.Role == deploy.Builder {
			if err := r.builderImages(ctx, pl, verify); err != nil {
				return err
			}
		}
		if verify && pl.Role == deploy.Agent {
			root := dataDir(r.Plan, pl) + "/tls/"
			proof, err := r.remote(ctx, r.Plan, pl, shell(operationsBinary(r.Plan))+" client-identity "+shell(cfgDir(r.Plan, pl)+"/ca.crt")+" "+shell(root+"client.crt")+" "+shell(root+"client.key")+" "+shell(pl.Instance)+" "+shell(string(identity.CallerAgent))+"\n")
			if err != nil {
				return err
			}
			serial := strings.TrimSpace(string(proof))
			var enrolled int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_certificates c JOIN agent_administration a ON a.agent_id=c.agent_id WHERE c.serial=$1 AND c.agent_id=$2 AND a.credential_revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM certificate_revocations r WHERE r.serial=c.serial)`, serial, pl.Instance).Scan(&enrolled); err != nil {
				return err
			}
			if enrolled != 1 {
				return fmt.Errorf("running agent certificate is not enrolled or was revoked")
			}
		}

	}
	return nil
}
func sortedFiles(files map[string][]byte) []string {
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func environment(env map[string]string) []byte {
	var keys []string
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key + "=\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(env[key]) + "\"\n")
	}
	return []byte(b.String())
}
func (r *Runner) controlPlaneAddresses(port string) string {
	var addresses []string
	for _, pl := range r.Plan.Placements {
		if pl.Role == deploy.ControlPlane {
			h, _ := r.Plan.Installation.Host(pl.Host)
			addresses = append(addresses, net.JoinHostPort(h.Network.Address, port))
		}
	}
	sort.Strings(addresses)
	return strings.Join(addresses, ",")
}

func (r *Runner) clientIdentity(ctx context.Context, pl deploy.Placement, class identity.CallerClass, signing *signkeys.Service, verify bool) (identity.ClientIdentityMaterial, error) {
	path := filepath.Join(r.Config.StateDirectory, r.Plan.Generation, pl.Instance+"-client.json")
	var material identity.ClientIdentityMaterial
	if err := privateJSON(path, &material); err == nil {
		pair, err := tls.X509KeyPair(material.CertPEM, material.KeyPEM)
		if err != nil {
			return material, err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return material, err
		}
		ca, err := signing.PublicBundle(ctx, signkeys.ScopeInternalCA)
		if err != nil {
			return material, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: func() time.Time {
			if verify && pl.Role == deploy.Agent {
				return leaf.NotBefore.Add(time.Minute)
			}
			return time.Now()
		}(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		valid := err == nil && leaf.Subject.CommonName == pl.Instance && len(leaf.Subject.OrganizationalUnit) == 1 && leaf.Subject.OrganizationalUnit[0] == string(class) && (time.Until(leaf.NotAfter) > 2*time.Hour || verify && pl.Role == deploy.Agent)
		if valid {
			material.CAPEM = ca
			return material, nil
		}
		if verify {
			return material, fmt.Errorf("component certificate is stale or has an incorrect caller identity")
		}
	} else if !os.IsNotExist(err) {
		return material, err
	} else if verify {
		return material, err
	}
	material, err := identity.IssueClientCertificate(ctx, signing, class, pl.Instance, 24*time.Hour)
	if err != nil {
		return material, err
	}
	b, _ := json.Marshal(material)
	return material, writePrivate(path, b)
}
func (r *Runner) databaseClient(ctx context.Context, pl deploy.Placement, db *sql.DB, files map[string][]byte, env map[string]string, verify bool) error {
	name := "component_" + strings.ReplaceAll(pl.Instance, "-", "_")
	var ca certificateBundle
	if err := privateJSON(filepath.Join(r.databasePKI(), "ca.json"), &ca); err != nil {
		return err
	}
	b, err := loadCertificate(filepath.Join(r.databasePKI(), name+".json"), name, &ca, nil, verify)
	if err != nil {
		return err
	}
	files["db-ca.crt"], files["db-client.crt"], files["db-client.key"] = ca.Certificate, b.Certificate, b.Key
	if !verify {
		for _, statement := range []string{"CREATE USER IF NOT EXISTS " + name, "GRANT ALL ON DATABASE " + r.Config.Database.Name + " TO " + name, "GRANT USAGE ON SCHEMA public TO " + name, "GRANT USAGE ON SCHEMA " + r.Config.Console.Schema + " TO " + name, "GRANT ALL ON ALL TABLES IN SCHEMA public TO " + name, "GRANT ALL ON ALL TABLES IN SCHEMA " + r.Config.Console.Schema + " TO " + name} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
	}
	u, err := verifiedDatabaseURL(name, r.databaseAddresses(), r.Config.Database.Name, cfgDir(r.Plan, pl)+"/db-ca.crt", cfgDir(r.Plan, pl)+"/db-client.crt", cfgDir(r.Plan, pl)+"/db-client.key")
	if err != nil {
		return err
	}
	files["database-url"] = []byte(u.String())
	return nil
}
func verifyWildcard(cert, key []byte, domain string) error {
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if time.Until(leaf.NotAfter) < 48*time.Hour {
		return fmt.Errorf("external wildcard certificate needs renewal")
	}
	if err := leaf.VerifyHostname("verification." + domain); err != nil {
		return err
	}
	return nil
}

func (r *Runner) reservations(ctx context.Context, verify bool) error {
	db, err := r.db(ctx, false)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent {
			continue
		}
		h, _ := r.Plan.Installation.Host(pl.Host)
		reserve := deploy.Resources{} // Already enforced by the native agent cgroups.
		hostType := "stable"
		if h.Reliability == "intermittent" {
			hostType = "intermittent"
		}
		if !verify {
			if err = r.yieldReservations(ctx, db, pl); err != nil {
				return err
			}
			err = adminTransaction(ctx, db, func(ctx context.Context, tx *sql.Tx) error {
				if _, err := journal.AgentRow(pl.Instance).Exec(ctx, tx, `INSERT INTO agent_registrations(id,name,region,failure_domain,reserved_cpu_millis,reserved_memory_mebibytes,created_at,updated_at) VALUES ($1,$1,$2,$3,$4,$5,statement_timestamp(),statement_timestamp()) ON CONFLICT(id) DO UPDATE SET reserved_cpu_millis=$4,reserved_memory_mebibytes=$5,region=$2,failure_domain=$3,updated_at=statement_timestamp()`, pl.Instance, h.Region, h.FailureDomain, reserve.CPUMillis, reserve.MemoryMiB); err != nil {
					return err
				}
				_, err := journal.AdministrationRow(pl.Instance).Exec(ctx, tx, `INSERT INTO agent_administration(agent_id,host_type,lifecycle_state,updated_at) VALUES ($1,$2,'enrolling',statement_timestamp()) ON CONFLICT(agent_id) DO UPDATE SET host_type=$2 WHERE agent_administration.lifecycle_state<>'retired'`, pl.Instance, hostType)
				return err
			})
			if err != nil {
				return err
			}
		}
		var cpu, ram int64
		var typ, life, domain string
		if err := db.QueryRowContext(ctx, `SELECT r.reserved_cpu_millis,r.reserved_memory_mebibytes,a.host_type,a.lifecycle_state,r.failure_domain FROM agent_registrations r JOIN agent_administration a ON a.agent_id=r.id WHERE r.id=$1`, pl.Instance).Scan(&cpu, &ram, &typ, &life, &domain); err != nil {
			return err
		}
		if cpu != reserve.CPUMillis || ram != reserve.MemoryMiB || typ != hostType || life == "retired" || domain != h.FailureDomain {
			return fmt.Errorf("agent reservations or admission differ from the applied plan")
		}
	}
	return nil
}
