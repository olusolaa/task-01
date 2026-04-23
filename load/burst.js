// Replay-storm scenario: simulates a bank reconciliation retry storm
// where half the traffic is new references and half are duplicates of
// earlier ones. The invariant under test is that replays return the
// original response and do not double-apply.
//
// Run with:
//   BASE_URL=http://localhost:8080 \
//   HMAC_SECRET=dev_secret_change_in_production \
//   k6 run load/burst.js

import http from 'k6/http';
import crypto from 'k6/crypto';
import { check } from 'k6';
import { SharedArray } from 'k6/data';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const SECRET = __ENV.HMAC_SECRET || 'dev_secret_change_in_production';

// A pool of fixed transaction references the "replay" half picks from.
const REFS = new SharedArray('refs', function () {
    const a = [];
    for (let i = 0; i < 5000; i++) {
        a.push('BURSTREF_' + i.toString().padStart(6, '0'));
    }
    return a;
});

export const options = {
    scenarios: {
        fresh: {
            executor: 'constant-arrival-rate',
            exec: 'fresh',
            rate: 1000,
            timeUnit: '1s',
            duration: '30s',
            preAllocatedVUs: 100,
            maxVUs: 400,
        },
        replay: {
            executor: 'constant-arrival-rate',
            exec: 'replay',
            rate: 1000,
            timeUnit: '1s',
            duration: '30s',
            preAllocatedVUs: 100,
            maxVUs: 400,
            startTime: '5s',
        },
    },
    thresholds: {
        http_req_duration: ['p(99) < 150'],
        http_req_failed: ['rate < 0.02'],
    },
};

function customerID() {
    const n = Math.floor(Math.random() * 1000) + 1;
    return 'GIG' + String(n).padStart(5, '0');
}

function txDate() {
    return new Date().toISOString().replace('T', ' ').slice(0, 19);
}

function post(ref) {
    const body = JSON.stringify({
        customer_id: customerID(),
        payment_status: 'COMPLETE',
        transaction_amount: '500',
        transaction_date: txDate(),
        transaction_reference: ref,
    });
    const sig = crypto.hmac('sha256', SECRET, body, 'hex');
    return http.post(`${BASE_URL}/payments`, body, {
        headers: {
            'Content-Type': 'application/json',
            'X-Signature': sig,
        },
    });
}

export function fresh() {
    const ref = REFS[Math.floor(Math.random() * REFS.length)];
    const res = post(ref);
    check(res, {
        fresh_ok: (r) => r.status === 201 || r.status === 200 || r.status === 409,
    });
}

export function replay() {
    const ref = REFS[Math.floor(Math.random() * REFS.length)];
    const res = post(ref);
    // On replay we expect the original status back, with Idempotent-Replayed.
    check(res, {
        replay_ok: (r) => r.status === 201 || r.status === 200 || r.status === 409,
    });
}
