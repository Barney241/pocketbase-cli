routerAdd("GET", "/api/pbctl/guard", (e) => {
  const readOnlyEmails = [__READ_ONLY_SUPERUSER_EMAILS__];
  const readOnly = !!e.auth && e.auth.isSuperuser() && readOnlyEmails.indexOf(e.auth.email().toLowerCase()) !== -1;
  return e.json(200, { readOnly: readOnly, enforcedBy: "server hook" });
});

routerUse((e) => {
  try {
    const readOnlyEmails = [__READ_ONLY_SUPERUSER_EMAILS__];
    const isReadOnly = (record) =>
      !!record && record.isSuperuser() && readOnlyEmails.indexOf(record.email().toLowerCase()) !== -1;

    const method = e.request.method;
    const path = e.request.url.path.replace(/\/{2,}/g, "/");

    if (path.indexOf("/api/backups/") === 0 && (method === "GET" || method === "HEAD")) {
      let fileTokenOwner = null;
      try {
        fileTokenOwner = e.app.findAuthRecordByToken(e.request.url.query().get("token"), "file");
      } catch (_) {
        fileTokenOwner = null;
      }
      if (isReadOnly(fileTokenOwner)) {
        throw new ForbiddenError("Refused by pbctl guard: a read-only superuser cannot download backups.");
      }
      return e.next();
    }

    if (!isReadOnly(e.auth)) {
      return e.next();
    }
    if (method === "GET" || method === "HEAD" || method === "OPTIONS") {
      let query = "";
      try {
        query = decodeURIComponent(e.request.url.rawQuery.replace(/\+/g, " ")).toLowerCase();
      } catch (_) {
        throw new ForbiddenError("Refused by pbctl guard: the query string could not be decoded.");
      }
      if (query.indexOf("tokenkey") !== -1 || query.indexOf("password") !== -1) {
        throw new ForbiddenError("Refused by pbctl guard: a read-only superuser cannot filter or sort by password or tokenKey.");
      }
      return e.next();
    }

    if (method === "POST" && path === "/api/realtime") {
      const topics = e.requestInfo().body.subscriptions || [];
      for (let i = 0; i < topics.length; i++) {
        if (!/^[A-Za-z0-9_]+(\/[A-Za-z0-9_*]+)?$/.test(String(topics[i]))) {
          throw new ForbiddenError("Refused by pbctl guard: a read-only superuser can only subscribe to <collection> or <collection>/<id>.");
        }
      }
      return e.next();
    }
    if (method === "POST" && (path === "/api/files/token" || /^\/api\/collections\/[^\/]+\/auth-refresh$/.test(path))) {
      return e.next();
    }

    throw new ForbiddenError("Refused by pbctl guard: this superuser is read-only; " + method + " " + path + " was refused.");
  } finally {
    e.request.url.rawQuery = e.request.url.rawQuery.replace(/(^|&)token=[^&]*/g, "$1token=REDACTED");
  }
});
