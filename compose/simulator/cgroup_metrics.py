"""Read real local cgroup v2 metrics. No Docker socket or Compose-file access.

This demo reports the current cgroup's quota, not an inferred effective limit
across hidden ancestors/cpuset. Unsupported or unreadable data is omitted.
"""
import json
import os
from pathlib import Path
import re


def unescape_mount(value):
    return re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), value)


def discover_cgroup(proc=Path('/proc/self')):
    current = next((line.split(':', 2)[2] for line in (proc / 'cgroup').read_text().splitlines()
                    if line.startswith('0::')), None)
    if current is None:
        raise ValueError('cgroup v2 is required by this demo collector')
    for line in (proc / 'mountinfo').read_text().splitlines():
        left, right = line.split(' - ', 1)
        if right.split()[0] != 'cgroup2':
            continue
        fields = left.split()
        root, mount = Path(unescape_mount(fields[3])), Path(unescape_mount(fields[4]))
        try:
            relative = Path(current).relative_to(root)
        except ValueError:
            continue
        if '..' in relative.parts:
            continue
        return mount / relative
    raise ValueError('current cgroup v2 directory is not visible')


def read_values(directory):
    """Return (name -> (type, value), failed source names), preserving partial results."""
    values, errors = {}, []
    def add(name, value, kind='gauge'):
        values['costrict_cgroup_' + name] = (kind, value)
    for source in ('cpu.max', 'cpu.stat', 'memory.max', 'memory.current'):
        try:
            raw = (directory / source).read_text().strip()
            if source == 'cpu.max':
                quota, period = raw.split()
                if int(period) <= 0 or (quota != 'max' and int(quota) <= 0):
                    raise ValueError('invalid CPU quota/period')
                add('cpu_limit_configured', int(quota != 'max'))
                if quota != 'max':
                    add('cpu_quota_cores', int(quota) / int(period))
            elif source == 'cpu.stat':
                stat = dict(line.split() for line in raw.splitlines())
                add('cpu_usage_seconds_total', int(stat['usage_usec']) / 1e6, 'counter')
                if 'throttled_usec' in stat:
                    add('cpu_throttled_seconds_total', int(stat['throttled_usec']) / 1e6, 'counter')
            elif source == 'memory.max':
                if raw != 'max' and int(raw) <= 0:
                    raise ValueError('invalid memory limit')
                add('memory_limit_configured', int(raw != 'max'))
                if raw != 'max':
                    add('memory_limit_bytes', int(raw))
            else:
                usage = int(raw)
                if usage < 0:
                    raise ValueError('negative memory usage')
                add('memory_usage_bytes', usage)
        except (OSError, ValueError, KeyError):
            errors.append(source)
    return values, errors


def render():
    hostname = os.uname().nodename
    labels = {'app': 'metrics-simulator', 'env': 'demo', 'instance_name': 'real-container',
              'sdk_instance_id': 'container-' + hostname, 'resource_id': 'container-' + hostname,
              'source': 'real-cgroup', 'limit_scope': 'local'}
    try:
        values, errors = read_values(discover_cgroup())
    except (OSError, ValueError):
        values, errors = {}, ['discovery']
    # A failed scrape source is distinct from a confirmed unlimited cgroup.
    values['costrict_cgroup_read_errors'] = ('gauge', len(errors))
    text_labels = ','.join(f'{key}={json.dumps(value)}' for key, value in sorted(labels.items()))
    lines = []
    for name, (kind, value) in values.items():
        lines.extend((f'# HELP {name} Real current cgroup v2 measurement; local limit scope.',
                      f'# TYPE {name} {kind}', f'{name}{{{text_labels}}} {value:.17g}'))
    return ('\n'.join(lines) + '\n').encode()
