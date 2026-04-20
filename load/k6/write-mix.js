// k6 write-mix generator. Targets the sidecar HTTP API (load/k6/server.go, Week 3)
// OR can use k6's `sql` extension to hit Postgres directly (xk6-sql-postgres).
// This file uses the HTTP variant because it's the one a recruiter can run
// without building a custom k6 binary.
//
// Run:
//   k6 run --vus 50 --duration 60s --env BASE=http://localhost:8085 load/k6/write-mix.js
//
// Default mix (override with env vars):
//   INSERT 70% | UPDATE 20% | DELETE 10%
//
// Until the sidecar lands (Week 3), run this against a minimal local HTTP
// shim that just forwards to psql. A placeholder `load/sidecar/` is on the
// roadmap in docs/plan.md.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const BASE      = __ENV.BASE      || 'http://localhost:8085';
const PCT_INS   = Number(__ENV.PCT_INS   || 70);
const PCT_UPD   = Number(__ENV.PCT_UPD   || 20);
// DELETE = remainder

const latInsert = new Trend('zdt_insert_latency_ms', true);
const latUpdate = new Trend('zdt_update_latency_ms', true);
const latDelete = new Trend('zdt_delete_latency_ms', true);
const errCount  = new Counter('zdt_errors_total');

export const options = {
    thresholds: {
        'http_req_failed':               ['rate<0.01'],    // <1% errors
        'http_req_duration{op:insert}':  ['p(99)<100'],
        'http_req_duration{op:update}':  ['p(99)<150'],
    },
    // Caller sets --vus / --duration. Reasonable defaults for a laptop demo:
    vus: 50,
    duration: '60s',
};

function randEmail(vu, it) {
    return `k6-${vu}-${it}-${Date.now()}@load.test`;
}

export default function () {
    const it = __ITER;
    const vu = __VU;
    const r = Math.random() * 100;

    if (r < PCT_INS) {
        const body = JSON.stringify({
            email: randEmail(vu, it),
            full_name: `K6 User ${vu}-${it}`,
            profile: { vu, iter: it, ts: Date.now() },
        });
        const res = http.post(`${BASE}/users`, body, {
            headers: { 'Content-Type': 'application/json' },
            tags: { op: 'insert' },
        });
        latInsert.add(res.timings.duration);
        if (!check(res, { 'insert 2xx': (r) => r.status >= 200 && r.status < 300 })) {
            errCount.add(1);
        }
    } else if (r < PCT_INS + PCT_UPD) {
        const res = http.patch(`${BASE}/users/random`, JSON.stringify({
            full_name: `Updated-${Date.now()}`,
        }), {
            headers: { 'Content-Type': 'application/json' },
            tags: { op: 'update' },
        });
        latUpdate.add(res.timings.duration);
        if (!check(res, { 'update 2xx-or-404': (r) => r.status < 500 })) {
            errCount.add(1);
        }
    } else {
        const res = http.del(`${BASE}/users/random`, null, { tags: { op: 'delete' } });
        latDelete.add(res.timings.duration);
        if (!check(res, { 'delete <500': (r) => r.status < 500 })) {
            errCount.add(1);
        }
    }
}
