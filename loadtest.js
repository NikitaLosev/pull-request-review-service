import http from 'k6/http';
import { check, sleep } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

export const options = {
  vus: 20,
  duration: '1m',
  thresholds: {
    http_req_duration: ['p(95)<300'], // 95% запросов быстрее 300мс
    http_req_failed: ['rate<0.001'], // 99.9% успешности
  },
};

const BASE = __ENV.TARGET || 'http://localhost:8080';
const TEAM = __ENV.TEAM || `loadtest-team-${__ENV.TEST_RUN_ID || uuidv4()}`;
const MEMBERS = [
  { user_id: `${TEAM}-u1`, username: 'alice', is_active: true },
  { user_id: `${TEAM}-u2`, username: 'bob', is_active: true },
  { user_id: `${TEAM}-u3`, username: 'carol', is_active: true },
  { user_id: `${TEAM}-u4`, username: 'dan', is_active: true },
];

export function setup() {
  const res = http.post(
    `${BASE}/team/add`,
    JSON.stringify({ team_name: TEAM, members: MEMBERS }),
    { headers: { 'Content-Type': 'application/json' } },
  );
  // TEAM_EXISTS для повторных прогонов не считается ошибкой нагрузки.
  if (![201, 400].includes(res.status)) {
    throw new Error(`Failed to seed team: ${res.status} ${res.body}`);
  }
}

export default function () {
  // Создаём PR с уникальным ID и автором из команды.
  const prID = `pr-${__VU}-${__ITER}-${Date.now()}`;
  const author = MEMBERS[0].user_id;

  let res = http.post(
    `${BASE}/pullRequest/create`,
    JSON.stringify({
      pull_request_id: prID,
      pull_request_name: 'perf-check',
      author_id: author,
    }),
    { headers: { 'Content-Type': 'application/json' } },
  );
  check(res, { 'create PR ok': (r) => r.status === 201 });

  // Быстрый merge для проверки идемпотентности /latency.
  res = http.post(
    `${BASE}/pullRequest/merge`,
    JSON.stringify({ pull_request_id: prID }),
    { headers: { 'Content-Type': 'application/json' } },
  );
  check(res, { 'merge ok': (r) => r.status === 200 });

  // Health для контроля доступности.
  const health = http.get(`${BASE}/health`);
  check(health, { 'health ok': (r) => r.status === 200 });

  sleep(0.2);
}
