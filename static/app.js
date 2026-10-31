(() => {
  document.querySelector("[data-print]")?.addEventListener("click", () => window.print());
  const report = document.querySelector("[data-scan-id]");
  const filterButtons = document.querySelectorAll("[data-filter]");

  if (filterButtons.length) {
    filterButtons.forEach((button) => {
      button.addEventListener("click", () => {
        filterButtons.forEach((item) => item.classList.toggle("active", item === button));
        const filter = button.dataset.filter;
        document.querySelectorAll("[data-page]").forEach((page) => {
          let visible = filter === "all";
          const items = page.querySelectorAll("[data-category]");
          items.forEach((item) => {
            const category = item.dataset.category;
            const status = item.dataset.status;
            const matches = filter === "all"
              || (filter === "issues" && status === "issue")
              || (filter === "warnings" && status === "warning")
              || (filter === "links" && category === "links")
              || (filter === "metadata" && category === "metadata")
              || (filter === "inconclusive" && ["inconclusive", "not_checked", "skipped"].includes(status));
            item.hidden = !matches;
            visible ||= matches;
          });
          page.hidden = !visible;
        });
      });
    });
  }

  if (!report) return;
  const scanId = report.dataset.scanId;
  const statusNode = document.getElementById("scan-status");
  if (!statusNode) return;

  const poll = async () => {
    try {
      const response = await fetch(`/scans/${encodeURIComponent(scanId)}/progress`, {
        headers: { Accept: "application/json" },
        credentials: "same-origin",
      });
      if (!response.ok) throw new Error(`Progress request returned ${response.status}`);
      const scan = await response.json();
      document.getElementById("pages-checked").textContent = scan.pages_checked;
      document.getElementById("pages-discovered").textContent = scan.pages_discovered;
      document.getElementById("links-checked").textContent = scan.links_checked;
      statusNode.textContent = `Scan ${scan.state}. ${scan.pages_checked} pages checked; ${scan.pages_discovered} discovered; ${scan.links_checked} link destinations checked.`;
      if (["completed", "cancelled", "failed", "interrupted"].includes(scan.state)) {
        window.location.reload();
        return;
      }
    } catch (error) {
      statusNode.textContent = `Progress could not be refreshed: ${error.message}`;
    }
    window.setTimeout(poll, 1800);
  };
  window.setTimeout(poll, 700);
})();
