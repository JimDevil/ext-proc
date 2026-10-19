// k6 load test against the local Envoy e2e stack.
//   docker compose -f test/e2e/docker-compose.yaml up -d --build
//   k6 run test/e2e/loadtest.js
import http from 'k6/http';
import { check } from 'k6';

export const options = {
  vus: 20,
  duration: '30s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<50', 'p(99)<100'],
  },
};

const body = JSON.stringify({
  model: 'glm-5.3',
  messages: [{ role: 'user', content: 'load test' }],
});

export default function () {
  const res = http.post('http://127.0.0.1:8080/v1/chat/completions', body, {
    headers: {
      'Content-Type': 'application/json',
      'x-api-key': 'e2e-test-key',
    },
  });
  check(res, { 'status is 200': (r) => r.status === 200 });
}
