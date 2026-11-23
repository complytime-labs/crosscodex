#!/usr/bin/env bash
# Bring-up half of the container-driven smoke test for the production compose
# stack. Brings up a fully isolated, ephemeral copy of the stack, imports a
# reference catalog through the gateway over mTLS using a host-built crosscodex
# CLI, and asserts it lists. It deliberately does NOT tear the stack down: teardown is
# the `deploy:smoke:down` task, so `deploy:smoke:up` can be run on its own to
# leave a live stack for debugging. The `deploy:smoke` task pairs this with a
# deferred `deploy:smoke:down` that runs whether this passes or fails.
#
# Isolation (so the smoke stack can run alongside a real production stack):
#   * COMPOSE_PROJECT_NAME    — separate project → own pod/containers/network
#                               and own (auto-namespaced) data volumes.
#   * CROSSCODEX_CERTS_VOLUME — prefix of the own per-service server key
#                               volumes (<prefix>-db, -nats, ...), generated fresh.
#   * CROSSCODEX_CLIENT_CERTS_VOLUME — own client key volume.
#   * CROSSCODEX_ATTESTATION_KEYS_VOLUME — own attestation key volume.
#   * CROSSCODEX_GATEWAY_PORT — own published host port (production keeps 50051).
#   * CERTS_DIR               — throwaway host dir for the smoke PKI.
#   * SMOKE_PRAXIS_PORT       — host-loopback port for Praxis's mTLS listener
#                               (LLM_GATEWAY=praxis only).
# All seven are consumed by deploy/compose.yaml plus the LLM gateway fragment
# (compose.gateway-<LLM_GATEWAY>.yaml) and by the deploy:* tasks; the
# defaults reproduce the production stack, so only the smoke test overrides them.
# The `deploy:smoke:*` tasks export these from the SMOKE_* taskfile vars; the
# defaults below only apply when this script is run directly.
set -euo pipefail

ROOT_DIR="${ROOT_DIR:-$(git rev-parse --show-toplevel)}"
OUT_DIR="${ROOT_DIR}/.test-output/deploy-smoke"
FIXTURE="${ROOT_DIR}/cmd/crosscodexd/testdata/oscal/e2e-minimal-catalog.json"
mkdir -p "${OUT_DIR}"

# Isolate this stack from any production stack sharing the same host. The values
# come from the environment (set by the deploy:smoke:* tasks); the defaults keep
# a direct `bash smoke_up.sh` invocation isolated too. CERTS_DIR is a fixed path
# under .test-output (not an mktemp) so deploy:smoke:down can find and remove the
# same PKI dir in a separate task invocation.
: "${COMPOSE_PROJECT_NAME:=crosscodex-smoke}"
: "${CROSSCODEX_CERTS_VOLUME:=crosscodex-smoke-certs}"
: "${CROSSCODEX_CLIENT_CERTS_VOLUME:=crosscodex-smoke-client-certs}"
: "${CROSSCODEX_ATTESTATION_KEYS_VOLUME:=crosscodex-smoke-attestation-keys}"
: "${CROSSCODEX_GATEWAY_PORT:=50151}"
: "${CERTS_DIR:=${OUT_DIR}/certs}"
: "${SMOKE_PRAXIS_PORT:=54443}"
export COMPOSE_PROJECT_NAME CROSSCODEX_CERTS_VOLUME CROSSCODEX_CLIENT_CERTS_VOLUME CROSSCODEX_ATTESTATION_KEYS_VOLUME CROSSCODEX_GATEWAY_PORT CERTS_DIR SMOKE_PRAXIS_PORT

# Seconds to wait for the analysis job to finish. 120 suits a GPU or remote
# model; CPU-only inference on the baked Ollama model needs more (several
# minutes). Checked here, before bring-up, so a typo fails in seconds.
: "${SMOKE_JOB_TIMEOUT:=120}"
if ! [[ "${SMOKE_JOB_TIMEOUT}" =~ ^[1-9][0-9]{0,5}$ ]]; then
	echo "ERROR: SMOKE_JOB_TIMEOUT=${SMOKE_JOB_TIMEOUT} is not a whole number of seconds from 1 to 999999. Set it to how long the analysis job may take, e.g. SMOKE_JOB_TIMEOUT=600 for CPU-only inference (default 120)." >&2
	exit 1
fi

# Bring the stack up offline under the identity above (provider selection,
# gateway config render, `task deploy:up`). Sourced, not run, so the provider
# and gateway it settles on (LLM_GATEWAY, OLLAMA_CHAT_MODEL, ...) and the podman
# runtime env reach the assertions below. Located next to this script rather
# than under ROOT_DIR, which the tooling suites point at a stub tree.
# shellcheck source=deploy/local_up.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../deploy/local_up.sh"

# Hardening baseline (issue #236): every running container of this project must
# have dropped all capabilities except its service's allowlist, run with
# no-new-privileges, and carry memory and pids limits; read-only services must
# have a read-only root filesystem. Capabilities and no-new-privileges are read
# from the kernel (/proc/<pid>/status of the container's PID 1), not from the
# engine's config: CapBnd is the bounding set cap_drop empties, and it is the
# meaningful check for non-root services, whose effective set is empty anyway.
# A service missing from the table fails, so a new service needs a decision.
# Values: "<read-only rootfs: ro|rw> <comma-separated allowed caps or ->".
declare -A hardening=(
	[db]="ro -" [nats]="ro -" [migrate]="ro -" [crosscodexd]="ro -"
	[litellm]="ro -" ["litellm-tls"]="ro -" [praxis]="ro -" [ollama]="ro -"
	["backup-init"]="ro CHOWN,DAC_OVERRIDE,FOWNER,FSETID" ["db-restore"]="rw -"
)
# Bit numbers from linux/capability.h for the caps the table may allow.
declare -A cap_bit=([CHOWN]=0 [DAC_OVERRIDE]=1 [DAC_READ_SEARCH]=2 [FOWNER]=3 [FSETID]=4)
engine="${CONTAINER_ENGINE:-podman}"
hardening_failures=0
hardening_fail() {
	echo "FAIL: hardening: service $1: $2" >&2
	hardening_failures=$((hardening_failures + 1))
}
mapfile -t smoke_containers < <("${engine}" ps --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" --format '{{.ID}}')
if [ "${#smoke_containers[@]}" -eq 0 ]; then
	echo "FAIL: hardening: no running containers found for project ${COMPOSE_PROJECT_NAME}." >&2
	exit 1
fi
for cid in "${smoke_containers[@]}"; do
	if ! info="$("${engine}" inspect --format \
		'{{index .Config.Labels "com.docker.compose.service"}} {{.State.Pid}} {{json .HostConfig.Memory}} {{json .HostConfig.PidsLimit}} {{json .HostConfig.ReadonlyRootfs}}' "${cid}")"; then
		echo "FAIL: hardening: '${engine} inspect ${cid}' failed (error above)." >&2
		exit 1
	fi
	read -r svc pid mem pids ro <<<"${info}"
	if [ -z "${svc}" ]; then
		hardening_fail "container ${cid}" "has no com.docker.compose.service label"
		continue
	fi
	if [ -z "${hardening[${svc}]+set}" ]; then
		hardening_fail "${svc}" "no expected hardening in test/deploy/smoke_up.sh; add the service to the hardening table"
		continue
	fi
	read -r want_ro want_caps <<<"${hardening[${svc}]}"
	allowed=0
	if [ "${want_caps}" != "-" ]; then
		IFS=, read -r -a names <<<"${want_caps}"
		for name in "${names[@]}"; do
			if [ -z "${cap_bit[${name}]+set}" ]; then
				echo "FAIL: hardening table allows ${name} for ${svc}, which cap_bit does not map; add its bit number." >&2
				exit 1
			fi
			allowed=$((allowed | (1 << cap_bit[${name}])))
		done
	fi
	status_file="/proc/${pid}/status"
	if ! cap_bnd="$(sed -n 's/^CapBnd:[[:space:]]*//p' "${status_file}")" || [ -z "${cap_bnd}" ]; then
		hardening_fail "${svc}" "cannot read CapBnd from ${status_file} (container PID ${pid})"
	elif ((0x${cap_bnd} & ~allowed)); then
		hardening_fail "${svc}" "capabilities beyond its allowlist: bounding set 0x${cap_bnd}, allowed $(printf '0x%x' "${allowed}") (${want_caps}); add cap_drop: [ALL] and cap_add only the allowed caps"
	fi
	if [ "$(sed -n 's/^NoNewPrivs:[[:space:]]*//p' "${status_file}")" != "1" ]; then
		hardening_fail "${svc}" "no-new-privileges not set; add security_opt: [no-new-privileges:true]"
	fi
	if ! [ "${mem}" -gt 0 ] 2>/dev/null; then
		hardening_fail "${svc}" "no memory limit (HostConfig.Memory=${mem}); add mem_limit"
	fi
	# podman gives every container a pids limit of 2048 by default, so only a
	# lower value shows that compose set one.
	if ! [ "${pids}" -gt 0 ] 2>/dev/null || [ "${pids}" -ge 2048 ]; then
		hardening_fail "${svc}" "no pids limit below the engine default 2048 (HostConfig.PidsLimit=${pids}); add pids_limit"
	fi
	if [ "${want_ro}" = "ro" ] && [ "${ro}" != "true" ]; then
		hardening_fail "${svc}" "root filesystem is writable; add read_only: true and a tmpfs for each path it writes"
	fi
done
if [ "${hardening_failures}" -gt 0 ]; then
	echo "FAIL: ${hardening_failures} hardening check(s) failed for project ${COMPOSE_PROJECT_NAME}." >&2
	exit 1
fi
echo "Hardening checks passed for ${#smoke_containers[@]} running container(s)."

# Key separation (issues #236, #298). Every key volume holds only its own
# files. Each service's server volume (<CERTS_VOLUME>-<service>) holds ca.pem
# plus that service's own certificate and key, as server.pem and
# server-key.pem; the client volume (migrate, crosscodexd) no CA, service or
# attestation key; the attestation volume (crosscodexd) no CA, service or
# client key. Each volume is mounted only by the services that use it.
#
# volume_files <volume> prints the volume's file names (C-sorted `ls -A`) and
# volume_digest <volume> <file> the file's sha256, both through a helper
# container (env as the entrypoint so key_absent's `test -e <path>` runs as the
# command; sh for the listing). A helper failure fails the smoke test.
#
# key_absent <what> <path> <command...> runs `<command...> test -e <path>`.
# test(1) exits 1 for an absent file; 0 (present) and any other status (no
# test binary, exec or run failure: nothing was checked) fail the smoke test.
key_absent() {
	local what="$1" path="$2" rc=0
	shift 2
	"$@" test -e "${path}" || rc=$?
	case "${rc}" in
	1) ;;
	0)
		echo "FAIL: certs volume: ${what} holds ${path}; the certs:load task in .taskfiles/deploy.yml must load only that volume's files into it." >&2
		exit 1
		;;
	*)
		echo "FAIL: certs volume: could not check ${path} in ${what} ('test -e' exited ${rc}; error above, if any)." >&2
		exit 1
		;;
	esac
}
image="crosscodex-db:${TAG:-latest}"
volume_files() {
	"${engine}" run --rm --entrypoint sh -v "$1:/v:ro,z" "${image}" -c 'LC_ALL=C ls -A /v | tr "\n" " "'
}
volume_digest() {
	"${engine}" run --rm --entrypoint sha256sum -v "$1:/v:ro,z" "${image}" "/v/$2" | cut -d' ' -f1
}
host_digest() {
	sha256sum "${CERTS_DIR}/$1" | cut -d' ' -f1
}

# The services with their own server key, and the compose service that mounts
# each one's volume (the litellm key is held by its stunnel sidecar).
cert_services=(db nats litellm praxis crosscodexd)
declare -A cert_mounter=([db]=db [nats]=nats [litellm]=litellm-tls [praxis]=praxis [crosscodexd]=crosscodexd)
# Every key file a service container must not see at /certs (besides the CA
# key): another service's, the client's and the attestation key. Its own is
# there as server-key.pem.
key_files=(client-key.pem attestation-key.pem)
for svc in "${cert_services[@]}"; do
	key_files+=("${svc}-key.pem")
done

# Each volume holds exactly its files, and each service volume's server-key.pem
# is the service's own key from the host PKI, so it holds no other key.
for svc in "${cert_services[@]}"; do
	vol="${CROSSCODEX_CERTS_VOLUME}-${svc}"
	want_listing=".fingerprint ca.pem server-key.pem server.pem "
	if ! got_listing="$(volume_files "${vol}")"; then
		echo "FAIL: certs volume: could not list ${vol} (error above)." >&2
		exit 1
	fi
	if [ "${got_listing}" != "${want_listing}" ]; then
		echo "FAIL: certs volume: ${vol} holds [${got_listing}], want [${want_listing}]; the certs:load task in .taskfiles/deploy.yml must load only ca.pem and the ${svc} certificate and key into it." >&2
		exit 1
	fi
	if ! got_digest="$(volume_digest "${vol}" server-key.pem)"; then
		echo "FAIL: certs volume: could not hash server-key.pem in ${vol} (error above)." >&2
		exit 1
	fi
	if [ "${got_digest}" != "$(host_digest "${svc}-key.pem")" ]; then
		echo "FAIL: certs volume: server-key.pem in ${vol} is not ${svc}-key.pem from the host PKI; a service may hold only its own key." >&2
		exit 1
	fi
done
for entry in "${CROSSCODEX_CLIENT_CERTS_VOLUME}:.fingerprint ca.pem client-key.pem client.pem " \
	"${CROSSCODEX_ATTESTATION_KEYS_VOLUME}:.fingerprint attestation-key.pem attestation-pub.pem "; do
	vol="${entry%%:*}"
	if ! got_listing="$(volume_files "${vol}")"; then
		echo "FAIL: certs volume: could not list ${vol} (error above)." >&2
		exit 1
	fi
	if [ "${got_listing}" != "${entry#*:}" ]; then
		echo "FAIL: certs volume: ${vol} holds [${got_listing}], want [${entry#*:}]; the certs:load task in .taskfiles/deploy.yml must load only that volume's files into it." >&2
		exit 1
	fi
done

# From inside each running service that has test(1): its own key is readable and
# no other key, nor the CA key, is reachable at the /certs path. crosscodexd is
# distroless (no test); the volume checks above and the mount check below cover it.
project_filter="label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}"
if [ "${LLM_GATEWAY}" = "praxis" ]; then
	running_services=(db nats praxis)
else
	running_services=(db nats litellm)
fi
for svc in "${running_services[@]}"; do
	mounter="${cert_mounter[${svc}]}"
	cid="$("${engine}" ps --filter "${project_filter}" \
		--filter "label=com.docker.compose.service=${mounter}" --format '{{.ID}}' | head -n1)"
	if [ -z "${cid}" ]; then
		echo "FAIL: certs volume: no running ${mounter} container in project ${COMPOSE_PROJECT_NAME} to check key access in." >&2
		exit 1
	fi
	if ! "${engine}" exec "${cid}" test -r /certs/server-key.pem; then
		echo "FAIL: certs volume: ${mounter} cannot read its own /certs/server-key.pem (error above, if any)." >&2
		exit 1
	fi
	for key in ca-key.pem "${key_files[@]}"; do
		key_absent "service ${mounter}" "/certs/${key}" "${engine}" exec "${cid}"
	done
done

# Mounts of every container in the project, exited ones included, so the
# one-shot migrate is checked too. Each volume may be mounted only by the
# services in volume_users, and each of those must be seen mounting it, so the
# check cannot pass on an empty or partial container list.
# Capture the list first: a failing ps inside a process substitution would not
# trip set -e and would leave the loop silently empty.
declare -A volume_users=(
	["${CROSSCODEX_CERTS_VOLUME}-db"]="db"
	["${CROSSCODEX_CERTS_VOLUME}-nats"]="nats"
	["${CROSSCODEX_CERTS_VOLUME}-litellm"]="litellm-tls"
	["${CROSSCODEX_CERTS_VOLUME}-praxis"]="praxis"
	["${CROSSCODEX_CERTS_VOLUME}-crosscodexd"]="crosscodexd"
	["${CROSSCODEX_CLIENT_CERTS_VOLUME}"]="migrate crosscodexd"
	["${CROSSCODEX_ATTESTATION_KEYS_VOLUME}"]="crosscodexd"
)
expected_mounts=(
	"db ${CROSSCODEX_CERTS_VOLUME}-db" "nats ${CROSSCODEX_CERTS_VOLUME}-nats"
	"crosscodexd ${CROSSCODEX_CERTS_VOLUME}-crosscodexd"
	"migrate ${CROSSCODEX_CLIENT_CERTS_VOLUME}" "crosscodexd ${CROSSCODEX_CLIENT_CERTS_VOLUME}"
	"crosscodexd ${CROSSCODEX_ATTESTATION_KEYS_VOLUME}"
)
if [ "${LLM_GATEWAY}" = "praxis" ]; then
	expected_mounts+=("praxis ${CROSSCODEX_CERTS_VOLUME}-praxis")
else
	expected_mounts+=("litellm-tls ${CROSSCODEX_CERTS_VOLUME}-litellm")
fi
if ! container_ids="$("${engine}" ps -a --filter "${project_filter}" --format '{{.ID}}')"; then
	echo "FAIL: certs volume: '${engine} ps -a' failed for project ${COMPOSE_PROJECT_NAME} (error above)." >&2
	exit 1
fi
mapfile -t all_containers <<<"${container_ids}"
declare -A seen_mount=()
for cid in "${all_containers[@]}"; do
	[ -n "${cid}" ] || continue
	if ! mounts="$("${engine}" inspect --format \
		'{{index .Config.Labels "com.docker.compose.service"}}{{range .Mounts}} {{.Name}}{{end}}' "${cid}")"; then
		echo "FAIL: certs volume: '${engine} inspect ${cid}' failed (error above)." >&2
		exit 1
	fi
	read -r svc names <<<"${mounts}"
	for name in ${names}; do
		[ -n "${volume_users[${name}]+set}" ] || continue
		case " ${volume_users[${name}]} " in
		*" ${svc} "*) seen_mount["${svc} ${name}"]=1 ;;
		*)
			echo "FAIL: certs volume: service ${svc} mounts ${name}; the only services allowed to mount it: ${volume_users[${name}]}." >&2
			exit 1
			;;
		esac
	done
done
for pair in "${expected_mounts[@]}"; do
	if [ -z "${seen_mount[${pair}]+set}" ]; then
		echo "FAIL: certs volume: mount check did not see '${pair}' (service volume) in project ${COMPOSE_PROJECT_NAME}; cannot prove which services mount which key volumes." >&2
		exit 1
	fi
done
echo "Each key volume holds only its own files and is mounted only by the services that use it: each service reads its own key and no other."

# No service key may act as a client identity (issues #236, #298): db, nats and
# the LLM gateway each hold a server key, and nats accepts any CA-signed client
# cert. Present each certificate to nats as a TLS client from a sibling
# container that holds what that service holds: its own volume and GID 10001
# (compose.yaml group_add). Every service certificate must get a TLS alert;
# client.pem, the positive control, must get PONG, so the check cannot pass on a
# broken probe. nats sends a plaintext INFO line before the TLS handshake, so
# `openssl s_client` cannot speak it; python3 (from the apache/age base of
# crosscodex-db, the image the volume checks above already use) can. Exit
# codes: 0 accepted, 3 refused by a TLS alert from nats, anything else means
# nothing was proven.
nats_probe='
import socket, ssl, sys
host, port, ca, cert, key = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]
def done(rc, msg):
    print(msg)
    sys.exit(rc)
try:
    ctx = ssl.create_default_context(cafile=ca)
    ctx.load_cert_chain(cert, key)
    raw = socket.create_connection((host, port), timeout=15)
    info = raw.makefile("rb").readline()
except Exception as e:
    done(2, "setup failed: %r" % e)
if not info.startswith(b"INFO "):
    done(2, "no NATS INFO line from %s:%d: %r" % (host, port, info[:80]))
buf = b""
try:
    tls = ctx.wrap_socket(raw, server_hostname=host)
    tls.sendall(b"CONNECT {\"verbose\":false,\"pedantic\":false,\"tls_required\":true}\r\nPING\r\n")
    while not any(l.startswith((b"PONG", b"-ERR")) for l in buf.split(b"\r\n")[:-1]):
        chunk = tls.recv(4096)
        if not chunk:
            done(2, "connection closed without PONG, -ERR or a TLS alert: %r" % buf[:200])
        buf += chunk
except ssl.SSLError as e:
    if "ALERT" in (e.reason or ""):
        done(3, "refused by TLS alert %s" % e.reason)
    done(2, "TLS error not sent by nats: %r" % e)
except Exception as e:
    done(2, "probe failed: %r" % e)
if any(l.startswith(b"PONG") for l in buf.split(b"\r\n")):
    done(0, "accepted (PONG)")
done(2, "nats answered with an error, not a TLS refusal: %r" % buf[:200])
'
nats_cid="$("${engine}" ps --filter "${project_filter}" \
	--filter "label=com.docker.compose.service=nats" --format '{{.ID}}' | head -n1)"
if [ -z "${nats_cid}" ]; then
	echo "FAIL: client identity: no running nats container in project ${COMPOSE_PROJECT_NAME} to probe." >&2
	exit 1
fi
# shellcheck disable=SC2016 # $name is a Go template variable, not shell.
if ! nats_network="$("${engine}" inspect --format '{{range $name, $net := .NetworkSettings.Networks}}{{$name}} {{end}}' "${nats_cid}")" ||
	[ -z "${nats_network// /}" ]; then
	echo "FAIL: client identity: cannot read the network of nats container ${nats_cid} (error above, if any)." >&2
	exit 1
fi
read -r nats_network _ <<<"${nats_network}"
# nats_presents <volume> <cert-base> sets probe_out (the probe's stdout; engine
# errors go to stderr) and probe_rc.
nats_presents() {
	probe_out="$("${engine}" run --rm --network "${nats_network}" --group-add 10001 \
		-v "$1:/v:ro,z" --entrypoint python3 "${image}" \
		-I -c "${nats_probe}" nats 4222 /v/ca.pem "/v/$2.pem" "/v/$2-key.pem")" && probe_rc=0 || probe_rc=$?
}
nats_presents "${CROSSCODEX_CLIENT_CERTS_VOLUME}" client
if [ "${probe_rc}" -ne 0 ]; then
	echo "FAIL: client identity: nats did not accept client.pem (probe exit ${probe_rc}: ${probe_out}); the probe cannot prove a service certificate is refused." >&2
	exit 1
fi
for svc in "${cert_services[@]}"; do
	# The litellm and praxis volumes are loaded whichever gateway runs, so the
	# one not deployed is probed too: its key is still on the host.
	nats_presents "${CROSSCODEX_CERTS_VOLUME}-${svc}" server
	case "${probe_rc}" in
	3) ;;
	0)
		echo "FAIL: client identity: nats accepted the ${svc} certificate as a client cert; every deploy server cert must be serverAuth-only (deploy/certgen/gen.go) so the services holding a server key cannot act as NATS clients." >&2
		exit 1
		;;
	*)
		echo "FAIL: client identity: could not check whether nats refuses the ${svc} certificate (probe exit ${probe_rc}: ${probe_out})." >&2
		exit 1
		;;
	esac
done
echo "nats accepts client.pem and refuses every service certificate as a client cert (${probe_out})."

# Hostname isolation (issue #298): each running service's certificate is valid
# for its own name and rejected for every other service's name, so a service
# that someone reaches by another service's hostname cannot pass for it. The
# probe (test/deploy/tls_name_probe.py) connects to the live service, so it
# checks the certificate actually served, not just the file on the host. The
# own name is probed first and must be accepted, so a refusal elsewhere means a
# hostname mismatch, not a dead service or a bad probe. localhost is a name only
# the host-facing services (praxis, crosscodexd) own.
# tls_name <service> <protocol> <port> <name> prints the probe output and sets
# probe_rc, presenting client.pem (the services require a client certificate).
tls_name() {
	probe_out="$("${engine}" run --rm --network "${nats_network}" --group-add 10001 \
		-v "${CROSSCODEX_CLIENT_CERTS_VOLUME}:/v:ro,z" \
		-v "${ROOT_DIR}/test/deploy/tls_name_probe.py:/probe.py:ro,z" \
		--entrypoint python3 "${image}" \
		-I /probe.py "$2" "$1" "$3" /v/ca.pem /v/client.pem /v/client-key.pem "$4")" && probe_rc=0 || probe_rc=$?
}
if [ "${LLM_GATEWAY}" = "praxis" ]; then
	gateway_service=praxis
else
	gateway_service=litellm
fi
declare -A service_protocol=([db]=postgres [nats]=nats [litellm]=tls [praxis]=tls [crosscodexd]=tls)
declare -A service_port=([db]=5432 [nats]=4222 [litellm]=4443 [praxis]=4443 [crosscodexd]=50051)
declare -A service_names=([db]="db" [nats]="nats" [litellm]="litellm" [praxis]="praxis localhost" [crosscodexd]="crosscodexd localhost")
probed_services=(db nats "${gateway_service}" crosscodexd)
all_names=(db nats litellm praxis crosscodexd localhost)
for svc in "${probed_services[@]}"; do
	for name in "${all_names[@]}"; do
		tls_name "${svc}" "${service_protocol[${svc}]}" "${service_port[${svc}]}" "${name}"
		case " ${service_names[${svc}]} " in
		*" ${name} "*) want_rc=0 ;;
		*) want_rc=4 ;;
		esac
		if [ "${probe_rc}" -ne "${want_rc}" ]; then
			echo "FAIL: hostname isolation: the ${svc} certificate checked for the name '${name}': probe exit ${probe_rc} (${probe_out}), want ${want_rc} (0 valid, 4 refused as not valid for that name). Each service certificate may name only its own service (deploy/certgen/gen.go)." >&2
			exit 1
		fi
	done
done
echo "Each service certificate is valid for its own names and refused for every other service's hostname."

# Reach the daemon exactly as deploy:up's health check does: a host process
# dialing the published gateway port over mTLS. In this rootless, nested-podman
# environment a sibling `podman run` container cannot reach the daemon -- joining
# its netns (--pod / --network container:) does not confer reachability, and
# rootless port publishing binds the port only in the host netns, not inside a
# --network host container. deploy:up's /healthz loop proves the
# host->published-port path works, so the smoke CLI uses the same reach.
#
# Build the CLI on the host. The deploy tasks already require a Go toolchain
# (deploy:certs runs `go run` for the PKI generator), so this adds no new
# dependency. CGO_ENABLED=0 mirrors the release binary.
CLI_BIN="${OUT_DIR}/crosscodex"
CGO_ENABLED=0 go build -o "${CLI_BIN}" "${ROOT_DIR}/cmd/crosscodex"

# The crosscodexd cert SANs cover localhost + 127.0.0.1; client.pem is the CLI's mTLS
# identity. Certs come from the host PKI dir that deploy:certs populated.
run_cli() {
	"${CLI_BIN}" \
		--endpoint "https://localhost:${CROSSCODEX_GATEWAY_PORT}" \
		--tls-ca "${CERTS_DIR}/ca.pem" \
		--tls-cert "${CERTS_DIR}/client.pem" \
		--tls-key "${CERTS_DIR}/client-key.pem" \
		"$@"
}

# Praxis path only: probe the gateway's mTLS listener directly (published on
# host loopback by compose.offline-praxis.yaml). The positive probe runs first,
# so a refused connection in the negative probes means the TLS handshake was
# rejected, not that the port is down.
if [ "${LLM_GATEWAY}" = "praxis" ]; then
	praxis_url="https://localhost:${SMOKE_PRAXIS_PORT}"
	embed_body='{"model":"embed","input":["smoke probe"]}'

	echo "Praxis probe: trusted client certificate is accepted ..."
	if ! curl -sSf --max-time 30 --cacert "${CERTS_DIR}/ca.pem" \
		--cert "${CERTS_DIR}/client.pem" --key "${CERTS_DIR}/client-key.pem" \
		-H 'Content-Type: application/json' -d "${embed_body}" \
		"${praxis_url}/v1/embeddings" >/dev/null; then
		echo "FAIL: Praxis rejected a request with the trusted client certificate." >&2
		exit 1
	fi

	echo "Praxis probe: no client certificate is refused ..."
	if curl -s --max-time 10 --cacert "${CERTS_DIR}/ca.pem" \
		-H 'Content-Type: application/json' -d "${embed_body}" \
		"${praxis_url}/v1/embeddings" >/dev/null 2>&1; then
		echo "FAIL: Praxis served a client that presented no certificate (mTLS not enforced)." >&2
		exit 1
	fi

	echo "Praxis probe: certificate from an untrusted CA is refused ..."
	# A second run of the deploy PKI generator mints an unrelated CA. It lives
	# under CERTS_DIR so deploy:smoke:down removes it with the smoke PKI.
	untrusted_dir="${CERTS_DIR}/untrusted"
	rm -rf "${untrusted_dir}"
	mkdir -p "${untrusted_dir}"
	go run "${ROOT_DIR}/deploy/certgen/gen.go" "${untrusted_dir}" >/dev/null
	# curl cannot tell a bad client cert file apart from a server refusal, so
	# verify the files exist first. With them present and the trusted probe
	# above passing, a refusal here is attributed to the untrusted CA.
	for f in client.pem client-key.pem; do
		if [ ! -s "${untrusted_dir}/${f}" ]; then
			echo "FAIL: untrusted-CA probe setup: ${untrusted_dir}/${f} is missing or empty." >&2
			echo "Check that 'go run deploy/certgen/gen.go ${untrusted_dir}' writes a client certificate and key." >&2
			exit 1
		fi
	done
	if curl -s --max-time 10 --cacert "${CERTS_DIR}/ca.pem" \
		--cert "${untrusted_dir}/client.pem" --key "${untrusted_dir}/client-key.pem" \
		-H 'Content-Type: application/json' -d "${embed_body}" \
		"${praxis_url}/v1/embeddings" >/dev/null 2>&1; then
		echo "FAIL: Praxis served a client certificate from an untrusted CA." >&2
		exit 1
	fi

	# Ask for a model Ollama really serves (the raw chat model name) but that is
	# not a Praxis alias. If Praxis forwarded unmapped models, Ollama would answer
	# 200, so a non-2xx here is Praxis's routing refusal, not an upstream one.
	echo "Praxis probe: unmapped model (${OLLAMA_CHAT_MODEL}) is rejected ..."
	unmapped_body="$(printf '{"model":"%s","messages":[{"role":"user","content":"hi"}],"max_tokens":1}' "${OLLAMA_CHAT_MODEL}")"
	if ! code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 --cacert "${CERTS_DIR}/ca.pem" \
		--cert "${CERTS_DIR}/client.pem" --key "${CERTS_DIR}/client-key.pem" \
		-H 'Content-Type: application/json' -d "${unmapped_body}" \
		"${praxis_url}/v1/chat/completions")"; then
		echo "FAIL: the unmapped-model request to ${praxis_url} did not complete (curl error above)." >&2
		echo "The trusted probe succeeded, so check whether the praxis container is still running." >&2
		exit 1
	fi
	case "${code}" in
	2??)
		echo "FAIL: Praxis forwarded an unmapped model (HTTP ${code})." >&2
		exit 1
		;;
	esac
	unmapped_code="${code}"

	# The body names a mapped model, so without the method gate Praxis would
	# route this GET to Ollama like a POST. Requiring the gate's own problem
	# detail as well as 405 keeps an upstream 405 from passing the probe.
	echo "Praxis probe: GET on a mapped path is refused with 405 ..."
	mapped_body='{"model":"default","messages":[{"role":"user","content":"hi"}],"max_tokens":1}'
	get_out="${OUT_DIR}/praxis-get-probe.json"
	if ! code="$(curl -sS -o "${get_out}" -w '%{http_code}' --max-time 30 --cacert "${CERTS_DIR}/ca.pem" \
		--cert "${CERTS_DIR}/client.pem" --key "${CERTS_DIR}/client-key.pem" \
		-X GET -H 'Content-Type: application/json' -d "${mapped_body}" \
		"${praxis_url}/v1/chat/completions")"; then
		echo "FAIL: the GET request to ${praxis_url}/v1/chat/completions did not complete (curl error above)." >&2
		echo "The trusted probe succeeded, so check whether the praxis container is still running." >&2
		exit 1
	fi
	if [ "${code}" != "405" ] || ! grep -q 'This gateway forwards only POST' "${get_out}"; then
		echo "FAIL: GET /v1/chat/completions got HTTP ${code} without the method gate's 405 (body in ${get_out})." >&2
		echo "Check that the static_response method gate (unless: methods: [POST]) precedes model_to_provider in deploy/praxis/config.offline.yaml.tmpl." >&2
		exit 1
	fi
	echo "Praxis probes passed (unmapped model got HTTP ${unmapped_code}, GET got HTTP ${code})."
fi

# Assert the import took effect (not just that the CLI exited 0): the gateway
# must return the imported catalog's ID. --plain disables color so the
# "Catalog ID:" and "Job ID:" lines parse cleanly below.
run_cli --plain catalog import "${FIXTURE}" | tee "${OUT_DIR}/import.log"
grep -q "Catalog imported" "${OUT_DIR}/import.log"
CATALOG_ID="$(sed -n 's/^Catalog ID: //p' "${OUT_DIR}/import.log" | head -n1)"
if [ -z "${CATALOG_ID}" ]; then
	echo "Could not parse a Catalog ID from the import output:" >&2
	cat "${OUT_DIR}/import.log" >&2
	exit 1
fi

# Import starts no analysis; start it explicitly. Gating on this job's terminal
# status is what makes the smoke test exercise the LLM path: the catalog row +
# controls persist during parse regardless of embeddings, so a `list` check
# alone passes against a broken LLM gateway/Ollama. The FULL_ANALYSIS job, by
# contrast, FAILs on insufficient embedding coverage (<80%), so only a working
# embed path drives it to COMPLETED.
run_cli --plain run start --source "${CATALOG_ID}" --target "${CATALOG_ID}" | tee "${OUT_DIR}/start.log"
JOB_ID="$(sed -n 's/^Job ID: //p' "${OUT_DIR}/start.log" | head -n1)"
if [ -z "${JOB_ID}" ]; then
	echo "Could not parse a Job ID from the run start output:" >&2
	cat "${OUT_DIR}/start.log" >&2
	exit 1
fi

# Poll the job to a terminal state: pass on COMPLETED, fail FAST on FAILED
# (printing the daemon's error so a broken LLM path is diagnosed here, not left
# to a misleading title-timeout). One attempt every 2s for SMOKE_JOB_TIMEOUT
# seconds; the default 120s is ample for a 3-control catalog's embedding +
# candidate pass on a GPU or remote model.
attempts=$(((SMOKE_JOB_TIMEOUT + 1) / 2))
echo "Waiting up to ${SMOKE_JOB_TIMEOUT}s for analysis job ${JOB_ID} to reach a terminal state ..."
for i in $(seq 1 "${attempts}"); do
	# Tolerate a transient CLI error mid-poll: an empty/garbled read yields no
	# Status line, so it is treated as not-ready and retried; the timeout below
	# governs give-up, and the last captured output is dumped for triage.
	run_cli --plain run status "${JOB_ID}" >"${OUT_DIR}/job.log" 2>&1 || true
	status="$(sed -n 's/^Status: //p' "${OUT_DIR}/job.log" | head -n1)"
	case "${status}" in
	COMPLETED)
		# Round-trip completeness: the analysis succeeded, so `catalog list` must
		# return the persisted catalog. The NAME column is intentionally blank --
		# the CLI import sends no catalog name and the server never derives one
		# from OSCAL metadata (see ISSUE.md) -- so match a data row beyond the
		# header rather than a title. This is a fresh, ephemeral DB, so exactly one
		# catalog row exists: header + one row = 2 non-empty lines.
		if ! run_cli --plain catalog list >"${OUT_DIR}/list.log" 2>&1; then
			echo "Job COMPLETED but 'catalog list' failed:" >&2
			cat "${OUT_DIR}/list.log" >&2
			exit 1
		fi
		if [ "$(grep -c . "${OUT_DIR}/list.log")" -lt 2 ]; then
			echo "Job COMPLETED but 'catalog list' shows no catalog row:" >&2
			cat "${OUT_DIR}/list.log" >&2
			exit 1
		fi
		cat "${OUT_DIR}/list.log"
		echo "SMOKE PASS"
		exit 0
		;;
	FAILED | CANCELLED)
		echo "Analysis job ${JOB_ID} ended ${status}; daemon report:" >&2
		cat "${OUT_DIR}/job.log" >&2
		exit 1
		;;
	esac
	# Progress so a slow job does not look hung (attempt N/attempts, ~2s apart).
	echo "  job ${status:-not-ready} (attempt ${i}/${attempts}); retrying in 2s ..."
	sleep 2
done
echo "Analysis job ${JOB_ID} did not reach a terminal state within ${SMOKE_JOB_TIMEOUT}s (raise SMOKE_JOB_TIMEOUT for slow inference); last status:" >&2
cat "${OUT_DIR}/job.log" >&2
exit 1
