#!/usr/bin/env bash
set -euo pipefail

# The chart's SKILLS_SRC_DIRS decides where the boot clone lands: the wrapper
# takes filepath.Dir of the FIRST entry as SkillsCloneDir, builds the new tree in
# "<SkillsCloneDir>.staging" (a sibling), and promotes it with os.Rename. Since
# Render now fails the boot on zero installed skills, a clone dir the pod cannot
# write is no longer a skill-less boot - it is a CrashLoopBackOff.
#
# Two values have already been wrong here and neither was caught by anything:
# "/templates/skills" outlived the baked skills it named (dropped 2026-06-28) and
# resolved the clone dir to "/templates", which uid 10001 cannot create; the
# replacement "/etc/wrapper/skills/skills" resolved it INSIDE the `files`
# ConfigMap mount, which kubelet mounts read-only. `ci / chart-validate` is off
# for this repo and only runs `helm lint` + `helm template >/dev/null` anyway, so
# neither render was ever asserted on. This is that assertion.
#
# Agent pods are not covered by this and do not need to be: the operator sets no
# SKILLS_SRC_DIRS (verified in tatara-operator/internal/agent/), so they take the
# Go default and /etc/wrapper is a plain image directory chowned to uid 10001
# there. This guard is about the published chart, whose consumers get the mount.

chart="charts/tatara-claude-code-wrapper"
render="$(helm template release "$chart")"

home="$(sed -n 's/^ENV HOME=\([^[:space:]]*\).*/\1/p' Dockerfile | head -1)"
if [ -z "${home}" ]; then
  echo "could not read 'ENV HOME=' from Dockerfile - this guard derives the writable root from it" >&2
  exit 1
fi

src="$(printf '%s\n' "${render}" |
  sed -n 's/^[[:space:]]*SKILLS_SRC_DIRS:[[:space:]]*"\?\([^"]*\)"\?[[:space:]]*$/\1/p' | head -1)"
if [ -z "${src}" ]; then
  echo "rendered chart sets no SKILLS_SRC_DIRS." >&2
  echo "It must set one: the Go default (/etc/wrapper/skills/skills) puts the clone dir inside" >&2
  echo "the read-only 'files' ConfigMap mount, so the boot clone fails and Render kills the pod." >&2
  exit 1
fi

first="${src%%:*}"
clone_dir="$(dirname "${first}")"
clone_parent="$(dirname "${clone_dir}")"

fail=0

# 1. The clone dir and its .staging sibling are created by the pod at boot, so
#    they must live under HOME - the one path the image both creates and chowns
#    to uid 10001 and that the chart never mounts over. HOME-only is deliberate:
#    /workspace is writable too, but it is the agent's repo checkout area, walked
#    by CommitAndPushAll and PVC-backed in this chart.
case "${clone_dir}/" in
"${home}"/*) ;;
*)
  echo "SKILLS_SRC_DIRS=${src} puts the skills clone dir at ${clone_dir}," >&2
  echo "which is not under the image's HOME (${home}) and so is not writable by uid 10001." >&2
  fail=1
  ;;
esac

mount_paths="$(printf '%s\n' "${render}" | sed -n 's/^[[:space:]]*mountPath:[[:space:]]*"\?\([^"]*\)"\?[[:space:]]*$/\1/p' | sort -u)"
while IFS= read -r mp; do
  [ -n "${mp}" ] || continue
  mp="${mp%/}"
  [ -n "${mp}" ] || continue

  # 2. A mount at or above the clone dir's PARENT shadows both the clone dir and
  #    the .staging sibling. ConfigMap and secret volumes are read-only, which is
  #    how /etc/wrapper/skills/skills bricked the chart.
  case "${clone_parent}/" in
  "${mp}"/*)
    echo "volumeMount ${mp} contains the skills clone dir's parent ${clone_parent}:" >&2
    echo "the boot clone writes both ${clone_dir} and ${clone_dir}.staging there." >&2
    fail=1
    ;;
  esac

  # 3. A mount exactly AT the clone dir is a different failure: the staging dir
  #    is then on another filesystem and promoteSkillsClone's os.Rename returns
  #    EXDEV on every boot.
  if [ "${mp}" = "${clone_dir}" ]; then
    echo "volumeMount ${mp} is the skills clone dir itself: os.Rename from" >&2
    echo "${clone_dir}.staging would cross a filesystem boundary (EXDEV) on every boot." >&2
    fail=1
  fi
done <<EOF
${mount_paths}
EOF

if [ "${fail}" -ne 0 ]; then
  exit 1
fi

echo "ok: skills clone dir ${clone_dir} is writable and unshadowed in the rendered chart"
