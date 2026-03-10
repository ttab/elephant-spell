(function() {
  var intervalMs = 25 * 60 * 1000; // 25 minutes

  function ping() {
    fetch("/api/keepalive", {
      credentials: "same-origin",
      redirect: "manual"
    }).then(function(resp) {
      // If the session has expired, howdah redirects to the login page.
      // With redirect: "manual" this shows up as an opaque redirect.
      if (resp.type === "opaqueredirect") {
        sessionStorage.removeItem("userinfo");
        window.location.reload();
      }
    }).catch(function() {
      // Network error — ignore, will retry on next interval.
    });
  }

  setInterval(ping, intervalMs);

  // Also ping when the user switches back to the tab.
  document.addEventListener("visibilitychange", function() {
    if (!document.hidden) {
      ping();
    }
  });
})();
