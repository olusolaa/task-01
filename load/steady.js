// Steady-state k6 scenario: 2,000 TPS for 60 seconds across ~1,000
// customers. Target is p99 < 100ms and error rate < 1%.
//
// Run with:
//   BASE_URL=http://localhost:8080 \
//   HMAC_SECRET=dev_secret_change_in_production \
//   k6 run load/steady.js

import http from 'k6/http';
import crypto from 'k6/crypto';
import { check } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const SECRET = __ENV.HMAC_SECRET || 'dev_secret_change_in_production';

export const options = {
    scenarios: {
        steady: {
            executor: 'constant-arrival-rate',
            rate: 2000,
            timeUnit: '1s',
            duration: '60s',
            preAllocatedVUs: 200,
            maxVUs: 800,
        },
    },
    thresholds: {
        http_req_duration: ['p(99) < 100'],
        http_req_failed: ['rate < 0.01'],
        'checks{check:status_is_success}': ['rate > 0.99'],
    },
};

function customerID() {
    const n = Math.floor(Math.random() * 1000) + 1;
    return 'GIG' + String(n).padStart(5, '0');
}

function txRef() {
    return 'LOADS' + Date.now() + '_' + __VU + '_' + __ITER + '_' +
        Math.random().toString(36).slice(2, 8);
}

function txDate() {
    return new Date().toISOString().replace('T', ' ').slice(0, 19);
}

export default function () {
    const body = JSON.stringify({
        customer_id: customerID(),
        payment_status: 'COMPLETE',
        transaction_amount: '1000',
        transaction_date: txDate(),
        transaction_reference: txRef(),
    });
    const sig = crypto.hmac('sha256', SECRET, body, 'hex');
    const res = http.post(`${BASE_URL}/payments`, body, {
        headers: {
            'Content-Type': 'application/json',
            'X-Signature': sig,
        },
    });

    // 201 = applied, 409 = overpayment (possible once balance nears zero
    // for a given customer), 200 = idempotent replay. Anything else is a
    // real failure.
    check(res, {
        status_is_success: (r) => r.status === 201 || r.status === 200 || r.status === 409,
    });
}
