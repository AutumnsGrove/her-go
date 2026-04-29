// her-router — Cloudflare Worker that routes Telegram webhook updates
// to either the prod instance (Mac Mini) or the dev instance (MacBook).
//
// Telegram → CF Worker → Cloudflare Tunnel → her webhook server
//
// The Worker validates the webhook secret, checks KV for an active dev
// session, and forwards the update. It returns 200 to Telegram immediately
// via ctx.waitUntil() so backend latency never causes Telegram timeouts.

export default {
  async fetch(request, env, ctx) {
    // Only accept POST — Telegram only sends POST for webhook updates.
    if (request.method !== "POST") {
      return new Response("method not allowed", { status: 405 });
    }

    // Validate the webhook secret. Telegram sends this header with every
    // update when a secret_token was provided during setWebhook. The CF
    // Worker also forwards it to the backend so telebot can double-check.
    const secret = request.headers.get("X-Telegram-Bot-Api-Secret-Token");
    if (secret !== env.WEBHOOK_SECRET) {
      return new Response("forbidden", { status: 403 });
    }

    // Determine routing target: dev instance (if active and fresh) or prod.
    const devActive = await env.HER_KV.get("dev_mode_active");
    let targetBase = env.PROD_URL;

    if (devActive === "true") {
      const devUrl = await env.HER_KV.get("dev_instance_url");
      const heartbeat = await env.HER_KV.get("dev_session_heartbeat");

      // A dev session is considered stale if the heartbeat is older than
      // 5 minutes — this catches cases where the MacBook crashed or the
      // dev session wasn't cleaned up properly. Stale sessions fall back
      // to prod so messages aren't lost.
      const staleMs = 5 * 60 * 1000;
      const isFresh = heartbeat && (Date.now() - parseInt(heartbeat, 10)) < staleMs;

      if (devUrl && isFresh) {
        targetBase = devUrl;
      }
    }

    // Read the body once — we need to forward it to the backend.
    const body = await request.arrayBuffer();

    // Fire-and-forget: forward the update to the backend. ctx.waitUntil()
    // keeps the Worker alive to complete the fetch even after we've
    // returned 200 to Telegram. If the backend is down, the forward
    // silently fails — Telegram will retry (it retries failed webhooks
    // with increasing delays up to ~1 hour).
    ctx.waitUntil(
      fetch(targetBase + "/webhook", {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Telegram-Bot-Api-Secret-Token": env.WEBHOOK_SECRET,
        },
        body,
      }).catch((err) => {
        // Log but don't throw — nothing we can do if the backend is
        // unreachable. Telegram's retry logic handles recovery.
        console.error("forward failed:", err.message, "target:", targetBase);
      })
    );

    return new Response("OK", { status: 200 });
  },
};
