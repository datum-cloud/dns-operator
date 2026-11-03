#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat >"$work/bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case " $* " in
  *' get pod '* )
    uid=member-uid
    [[ " $* " != *' get pod probe '* ]] || uid=probe-uid
    [[ ${CASE:-} != replaced ]] || uid=replacement
    image=$IMAGE; [[ ${CASE:-} != wrong-image ]] || image="wrong"
    jq -n --arg uid "$uid" --arg image "$image" '{metadata:{uid:$uid},spec:{containers:[{name:"agent",image:$image}],volumes:[{configMap:{name:"config"}}]},status:{phase:"Running",containerStatuses:[{name:"agent",ready:true}]}}'
    ;;
  *' get configmap '* )
    if [[ ${CASE:-} == wrong-configmap ]]; then jq '.data.config="changed"' "$FIXTURE/config.json"; else cat "$FIXTURE/config.json"; fi ;;
  *' sha256sum '* )
    number=3; [[ ${CASE:-} != wrong-config ]] || number=4
    printf '%064d  /config.json\n' "$number" ;;
  *' -role activation-check '* )
    [[ ${CASE:-} != pending ]] || exit 1
    count=$(cat "$FIXTURE/calls" 2>/dev/null || echo 0)
    echo $((count+1)) >"$FIXTURE/calls"
    if [[ ${CASE:-} == changed && $count -gt 0 ]]; then printf '%064d\n' 2; else printf '%064d\n' 1; fi
    ;;
  *' dig '* )
    status=NOERROR; flags='qr aa rd ra'; name=fixture.internal.; answer=192.0.2.10
    if [[ ${CASE:-} == tcp-failed && " $* " == *' +tcp '* ]]; then status=SERVFAIL; fi
    [[ ${CASE:-} != wrong ]] || answer=192.0.2.99
    [[ ${CASE:-} != foreign ]] || name=other.internal.
    [[ ${CASE:-} != truncated ]] || flags='qr aa tc'
    echo ";; ->>HEADER<<- opcode: QUERY, status: $status, id: 1"
    echo ";; flags: $flags; QUERY: 1, ANSWER: 1"
    [[ ${CASE:-} == empty ]] || echo "$name 30 IN A $answer"
    ;;
  *) echo "unexpected mock invocation: $*" >&2; exit 1 ;;
esac
MOCK
chmod +x "$work/bin/kubectl"
export PATH="$work/bin:$PATH"
export IMAGE="ghcr.io/test/dns@sha256:$(printf '%064d' 1)"
for CASE in pass pending changed replaced tcp-failed wrong foreign truncated empty no-members no-probes wrong-config wrong-image wrong-configmap; do
  export CASE FIXTURE="$work/$CASE"
  mkdir "$FIXTURE"
  printf '{"metadata":{"uid":"cm-uid"},"data":{"config":"{}"}}\n' >"$FIXTURE/config.json"
  hash=$(jq -S -c .data "$FIXTURE/config.json" | shasum -a 256 | awk '{print $1}')
  jq -n --arg image "$IMAGE" --arg hash "$hash" '{context:"mock",members:[{namespace:"dns",pod:"member",uid:"member-uid",container:"agent",config:"/config.json",config_sha256:("0000000000000000000000000000000000000000000000000000000000000003"),images:{agent:$image},configmap:{name:"config",uid:"cm-uid",sha256:$hash}}],probes:[{namespace:"consumer",pod:"probe",uid:"probe-uid",container:"agent",resolver:"fd53::54",name:"fixture.internal.",type:"A",answers:["192.0.2.10"]}]}' >"$FIXTURE/spec.json"
  if [[ $CASE == no-members ]]; then jq '.members=[]' "$FIXTURE/spec.json" >"$FIXTURE/new"; mv "$FIXTURE/new" "$FIXTURE/spec.json"; fi
  if [[ $CASE == no-probes ]]; then jq '.probes=[]' "$FIXTURE/spec.json" >"$FIXTURE/new"; mv "$FIXTURE/new" "$FIXTURE/spec.json"; fi
  if "$here/before-cleanup.sh" "$FIXTURE/spec.json" "$FIXTURE/evidence" >"$FIXTURE/output" 2>&1; then
    [[ $CASE == pass ]] || { cat "$FIXTURE/output"; echo "unexpected pass: $CASE" >&2; exit 1; }
  else
    [[ $CASE != pass ]] || { cat "$FIXTURE/output"; echo "unexpected failure: $CASE" >&2; exit 1; }
  fi
  echo "passed: $CASE"
done
