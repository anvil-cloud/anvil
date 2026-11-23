// Anvil viewer-request function — CloudFront Function (cloudfront-js-2.0).
//
// One function per distribution, because CloudFront allows a single
// viewer-request function per cache behavior. It does up to two things, in
// this order:
//
// 1. Origin protection (when GUARD_KVS is set): only requests carrying the
//    secret x-origin-secret header, added by the CDN/proxy in front of
//    CloudFront, are let through. Everything else gets a 403 before reaching
//    the cache or the origin. The secret lives in a CloudFront KeyValueStore,
//    not in this code, so reading the function doesn't reveal it. The header is
//    removed after the check so the app never receives (and can't
//    accidentally log) the secret.
//
// 2. apex <-> www redirect (when REDIRECT_HOST is set): requests for
//    REDIRECT_HOST get a 301 to the same path and query on CANONICAL_HOST.
//    It runs after the guard, so it can't be used to reach the site around
//    the proxy.
import cf from 'cloudfront';

const GUARD_KVS = '__KVS_ID__';
const REDIRECT_HOST = '__REDIRECT_HOST__';
const CANONICAL_HOST = '__CANONICAL_HOST__';
const HEADER = 'x-origin-secret';

// KeyValueStore keys whose values are accepted. Rotation without downtime adds
// a second key here (accept old and new while the proxy is updated).
const SECRET_KEYS = ['origin-secret'];

// Constant-time comparison so response timing doesn't reveal how much of the
// secret matched.
function safeEqual(a, b) {
  if (typeof a !== 'string' || typeof b !== 'string' || a.length !== b.length) {
    return false;
  }
  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

async function hasSecret(request) {
  const provided = request.headers[HEADER];
  if (!provided) {
    return false;
  }
  const kvs = cf.kvs(GUARD_KVS);
  for (let i = 0; i < SECRET_KEYS.length; i++) {
    let expected;
    try {
      expected = await kvs.get(SECRET_KEYS[i]);
    } catch (e) {
      continue; // key not present
    }
    if (safeEqual(provided.value, expected)) {
      return true;
    }
  }
  return false;
}

function queryString(qs) {
  const parts = [];
  for (const key in qs) {
    const entry = qs[key];
    const values = entry.multiValue ? entry.multiValue : [entry];
    for (let i = 0; i < values.length; i++) {
      parts.push(values[i].value === '' ? key : key + '=' + values[i].value);
    }
  }
  return parts.length ? '?' + parts.join('&') : '';
}

async function handler(event) {
  const request = event.request;

  if (GUARD_KVS !== '') {
    if (!(await hasSecret(request))) {
      return { statusCode: 403, statusDescription: 'Forbidden' };
    }
    delete request.headers[HEADER];
  }

  if (REDIRECT_HOST !== '') {
    const host = request.headers.host ? request.headers.host.value.toLowerCase() : '';
    if (host === REDIRECT_HOST) {
      return {
        statusCode: 301,
        statusDescription: 'Moved Permanently',
        headers: {
          location: { value: 'https://' + CANONICAL_HOST + request.uri + queryString(request.querystring) },
        },
      };
    }
  }

  return request;
}
