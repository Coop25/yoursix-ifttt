// Puts the demo into the state for one README screenshot, picked by
// ?shot=NAME. Loaded after app.js by demo/screenshots.sh only; the Pages
// demo never includes it.
(() => {
  "use strict";
  const shot = new URLSearchParams(location.search).get("shot");
  if (!shot) return;
  const wait = (ms) => new Promise((r) => setTimeout(r, ms));
  const until = async (fn) => { for (let i = 0; i < 100 && !fn(); i++) await wait(50); };

  const shots = {
    async rules() {
      showTab("rules");
      await until(() => state.rules?.length);
      // One rule mid-run, so the card shows live progress.
      await api("POST", `/api/rules/${state.rules.find((r) => r.name.startsWith("CAM-01")).id}/test`, {});
      await wait(1500);
      await loadRules();
    },
    async events() {
      // A failing run, so the log shows the failure handler.
      const cam = (await api("GET", "/api/rules")).find((r) => r.name.startsWith("CAM-01"));
      const r = cleanRule(cam);
      r.actions[0] = { ...r.actions[0], url: "http://cam-offline.local/axis-cgi/io/port.cgi?action=2:/" };
      r.cooldownSeconds = 0;
      await api("PUT", `/api/rules/${cam.id}`, r);
      await api("POST", "/api/simulate", { eventTopic: "device", eventCode: "vmd", eventDescription: "Video motion detection", eventOccurred: new Date().toISOString(), eventData: { deviceName: "CAM-01", siteName: "HQ", deviceId: "d1", videoSourceId: "0" } });
      await wait(1500);
      showTab("events");
      await until(() => state.events?.length);
      $("#events-q").value = "device:CAM-01";
      state.events = [];
      await loadEvents();
      $("#events-body tr.ev").click();
      await wait(500);
    },
    async editor() {
      showTab("rules");
      await until(() => state.rules?.length);
      try { localStorage.setItem("y6.previewTemplates", "1"); } catch { /* ignore */ }
      $("#f-preview-on").checked = true;
      const rule = state.rules.find((r) => r.name.startsWith("Front door"));
      rule.scheduleId = state.schedules.find((s) => s.name === "Weeknights")?.id;
      await openEditor(rule);
      await wait(800);
    },
    async actions() {
      showTab("requests");
      await until(() => state.requests?.length);
    },
    async schedules() {
      showTab("schedules");
      await until(() => state.schedules?.length);
    },
    async "schedule-editor"() {
      showTab("schedules");
      await until(() => state.schedules?.length);
      openScheduleEditor(state.schedules.find((s) => s.name === "Business hours"));
      windowRow({ days: ["mon"], from: "09:00", to: "12:00" });
      refreshScheduleEditor();
    },
    async history() {
      showTab("history");
      await until(() => $$(".rev").length);
      const edited = $$(".rev").find((el) => el.textContent.includes("edited"));
      $(".rev-head", edited).click();
      await wait(500);
    },
    async secrets() { showTab("secrets"); await wait(500); },
    async setup() { showTab("setup"); await wait(500); },
  };

  window.addEventListener("load", async () => {
    // The README shows the app, not the demo's banner.
    document.getElementById("demo-banner")?.remove();
    await until(() => state.me && state.catalog);
    await shots[shot]?.();
    document.activeElement?.blur(); // no focus rings in the pictures
    document.documentElement.dataset.shotReady = "1";
  });
})();
