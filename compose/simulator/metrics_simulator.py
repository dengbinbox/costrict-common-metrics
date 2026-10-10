#!/usr/bin/env python3
"""Synthetic Go SDK metrics, not measurements of this Python process.

Standard library only. Counters advance in a background clock, never on scrape.
Three stable demo identities are exposed separately for Prometheus discovery.
"""
import argparse
import cgroup_metrics
import json
import logging
import math
import os
from pathlib import Path
import random
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

WORKERS = ('worker-a', 'worker-b', 'worker-c')
BUCKETS = (0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5)
ROUTES = ('/ping', '/orders', '/reports')
MIB = 1024 * 1024


class Simulation:
    def __init__(self, data_dir):
        self.path = Path(data_dir) / 'state.json'
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.rng = random.Random(42)
        self.state = {'version': 1, 'elapsed': 0.0, 'started': time.time(), 'workers': {}}
        for name in WORKERS:
            self.state['workers'][name] = {
                'cpu': 0.0, 'allocated': 0.0, 'gc_count': 0.0, 'gc_sum': 0.0,
                'routes': {route: {'ok': 0, 'error': 0, 'sum': 0.0,
                                   'buckets': [0] * len(BUCKETS)} for route in ROUTES},
            }
        if self.path.exists():
            self.state = json.loads(self.path.read_text())
            if self.state.get('version') != 1 or set(self.state['workers']) != set(WORKERS):
                raise ValueError('Unsupported simulator checkpoint; move state.json aside to reset')
        self.updated_at = time.time()
        self.thread = threading.Thread(target=self.run, daemon=True)

    def profile(self, index):
        phase = self.state['elapsed'] / 35 + index * 1.7
        wave = (math.sin(phase) + 1) / 2
        # worker-a is consistently busier; all profiles vary over time.
        base = (0.60, 0.23, 0.08)[index]
        return base + wave * (0.55, 0.22, 0.12)[index], wave

    def step(self, dt):
        with self.lock:
            self.state['elapsed'] += dt
            for i, name in enumerate(WORKERS):
                w = self.state['workers'][name]
                cpu, wave = self.profile(i)
                gc_rate = 1 + cpu * 3
                w['cpu'] += cpu * dt
                w['allocated'] += (3 + cpu * 12) * MIB * dt
                gc_events = round(gc_rate * dt)
                w['gc_count'] += gc_events
                w['gc_sum'] += gc_events * (0.00012 + wave * 0.00035)
                for j, route in enumerate(ROUTES):
                    r = w['routes'][route]
                    n = max(1, round((65 - i * 18) * (0.5 + wave) / (j + 1) * dt))
                    for _ in range(n):
                        duration = self.rng.lognormvariate(math.log(0.012 * (j + 1) * (1 + cpu)), 0.8)
                        failed = self.rng.random() < (0.005 + (0.035 * wave if i == 0 else 0.006))
                        r['error' if failed else 'ok'] += 1
                        r['sum'] += duration
                        for k, bound in enumerate(BUCKETS):
                            if duration <= bound:
                                r['buckets'][k] += 1
            self.updated_at = time.time()

    def checkpoint(self):
        with self.lock:
            snapshot = json.dumps(self.state)
        temporary = self.path.with_suffix('.tmp')
        temporary.write_text(snapshot)
        os.replace(temporary, self.path)

    def run(self):
        last = time.monotonic()
        saved = last
        while not self.stop.wait(1):
            now = time.monotonic()
            self.step(now - last)
            last = now
            if now - saved >= 5:
                self.checkpoint()
                saved = now

    def render(self, selected):
        lines, declared = [], set()
        with self.lock:
            def emit(name, value, labels, kind='gauge', family=None):
                family = family or name
                if family not in declared:
                    lines.extend((f'# HELP {family} Synthetic demo {family}; not real process measurements.',
                                  f'# TYPE {family} {kind}'))
                    declared.add(family)
                label_text = ','.join(f'{k}={json.dumps(str(v))}' for k, v in sorted(labels.items()))
                lines.append(f'{name}{{{label_text}}} {value:.17g}')

            for i, worker in enumerate(WORKERS):
                if worker not in selected:
                    continue
                labels = {'app': 'python-metrics-example', 'env': 'demo', 'sdk_instance_id': f'demo-{worker}',
                          'instance_name': worker}
                w = self.state['workers'][worker]
                cpu, wave = self.profile(i)
                heap = ((155, 92, 48)[i] + wave * 45) * MIB
                rss = heap + (180, 120, 75)[i] * MIB
                gauges = {
                    'process_start_time_seconds': self.state['started'],
                    'process_resident_memory_bytes': rss,
                    'process_virtual_memory_bytes': (1500 + i * 100) * MIB,
                    'process_open_fds': int(70 + cpu * 180), 'process_max_fds': 1024,
                    'go_goroutines': int(60 + cpu * 150), 'go_threads': int(8 + cpu * 12),
                    'go_memstats_heap_alloc_bytes': heap,
                    'go_memstats_heap_inuse_bytes': heap * 1.15,
                    'go_memstats_heap_idle_bytes': 70 * MIB,
                    'go_memstats_heap_released_bytes': 40 * MIB,
                    'go_memstats_sys_bytes': heap * 1.15 + 110 * MIB,
                    'go_memstats_heap_objects': int(heap / 160),
                    'go_memstats_stack_inuse_bytes': (3 + cpu * 4) * MIB,
                    'go_memstats_next_gc_bytes': heap * 1.5,
                    'costrict_sdk_collection_timestamp_seconds': self.updated_at,
                    'costrict_sdk_collection_duration_seconds': 0.001 + wave * 0.003,
                }
                for name, value in gauges.items():
                    emit(name, value, labels)
                # One simulated resource per worker; never sum duplicate cgroup collectors.
                resource = dict(labels, resource_id=f'demo-cgroup-{worker}')
                emit('costrict_cgroup_cpu_usage_seconds_total', w['cpu'] * 1.06, resource, 'counter')
                emit('costrict_cgroup_memory_usage_bytes', rss + (25 + wave * 20) * MIB, resource)
                cpu_limit = (1.5, 0.5, None)[i]
                memory_limit = (512 * MIB, 384 * MIB, None)[i]
                emit('costrict_cgroup_cpu_limit_configured', int(cpu_limit is not None), resource)
                emit('costrict_cgroup_memory_limit_configured', int(memory_limit is not None), resource)
                if cpu_limit is not None:
                    emit('costrict_cgroup_cpu_quota_cores', cpu_limit, resource)
                if memory_limit is not None:
                    emit('costrict_cgroup_memory_limit_bytes', memory_limit, resource)
                emit('go_info', 1, dict(labels, version='go1.25.0-demo'))
                emit('costrict_sdk_build_info', 1, dict(labels, version='simulator-v1'))
                emit('process_cpu_seconds_total', w['cpu'], labels, 'counter')
                emit('go_memstats_alloc_bytes_total', w['allocated'], labels, 'counter')
                for suffix in ('count', 'sum'):
                    emit('go_gc_duration_seconds_' + suffix, w['gc_' + suffix], labels,
                         'summary', 'go_gc_duration_seconds')
                for collector in ('go', 'process', 'custom', 'registry'):
                    emit('costrict_sdk_collection_errors_total', 0, dict(labels, collector=collector), 'counter')
                emit('costrict_sdk_collection_skipped_total', 0, dict(labels, reason='busy'), 'counter')
                for route, r in w['routes'].items():
                    rl = dict(labels, route=route, method='GET')
                    for status, key in (('200', 'ok'), ('500', 'error')):
                        emit('billing_http_requests_total', r[key], dict(rl, status=status), 'counter')
                    family = 'billing_http_request_duration_seconds'
                    for bound, count in zip(BUCKETS, r['buckets']):
                        emit(family + '_bucket', count, dict(rl, le=str(bound)), 'histogram', family)
                    emit(family + '_bucket', r['ok'] + r['error'], dict(rl, le='+Inf'), 'histogram', family)
                    emit(family + '_count', r['ok'] + r['error'], rl, 'histogram', family)
                    emit(family + '_sum', r['sum'], rl, 'histogram', family)
        # Keep all samples of a family together, including across simulated instances.
        headers, samples = {}, {}
        for line in lines:
            if line.startswith('#'):
                family = line.split()[2]
                headers.setdefault(family, []).append(line)
            else:
                name = line.split('{', 1)[0]
                family = name
                for suffix in ('_bucket', '_sum', '_count'):
                    if name.endswith(suffix) and name[:-len(suffix)] in headers:
                        family = name[:-len(suffix)]
                        break
                samples.setdefault(family, []).append(line)
        return ('\n'.join(line for family in headers for line in headers[family] + samples[family]) + '\n').encode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--port', type=int, default=8888)
    parser.add_argument('--data-dir', default='data/simulator')
    args = parser.parse_args()
    sim = Simulation(args.data_dir)

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            path = urlsplit(self.path).path
            if path == '/health':
                body, content_type = b'ok\n', 'text/plain'
            elif path == '/metrics/real':
                body = cgroup_metrics.render()
                content_type = 'text/plain; version=0.0.4; charset=utf-8'
            elif path == '/metrics' or path in [f'/metrics/{w}' for w in WORKERS]:
                body = sim.render(WORKERS if path == '/metrics' else [path.rsplit('/', 1)[1]])
                content_type = 'text/plain; version=0.0.4; charset=utf-8'
            elif path == '/':
                body = ('Metrics SDK simulator (synthetic data, not real Go processes)\n'
                        '/metrics/real - real container cgroup v2 metrics\n'
                        '/metrics - all 3 simulated workers\n' + ''.join(f'/metrics/{w}\n' for w in WORKERS)).encode()
                content_type = 'text/plain; charset=utf-8'
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header('Content-Type', content_type)
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, fmt, *args):
            if args and str(args[1]) != '200':
                logging.info(fmt, *args)

    server = ThreadingHTTPServer(('0.0.0.0', args.port), Handler)
    def shutdown(_signum, _frame):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    sim.thread.start()
    logging.warning('Synthetic metrics listening on :%s; checkpoint %s', args.port, sim.path)
    try:
        server.serve_forever()
    finally:
        sim.stop.set()
        sim.thread.join()
        sim.checkpoint()
        server.server_close()


if __name__ == '__main__':
    main()
