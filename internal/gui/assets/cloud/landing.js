// magpie cloud — the public landing page. It reads only the public policy
// and the current session, and draws entry points that match them: a signed
// in reader opens the console, a new one may create an account when the
// deployment allows it, and a closed deployment offers sign-in alone. No
// provider or payment behavior lives here.
"use strict";

(function () {
  const {
    byId, h, clear, show, setPipeText, t, onChange,
    fetchMe, fetchPolicy
  } = window.magpieCloudShared;

  let user = null;
  let policyState = { known: false, policy: Object.assign({}, window.magpieCloudShared.POLICY_FALLBACK), reason: null };

  function actionButton(labelKey, href, kind, big) {
    const link = h("a", "btn" + (kind ? " " + kind : "") + (big ? " big" : ""), t(labelKey));
    link.href = href;
    return link;
  }

  function drawActions(node, big) {
    if (!node) return;
    clear(node);
    if (user) {
      node.append(actionButton("navOpenConsole", "/app", "primary", big));
      return;
    }
    const { policy } = policyState;
    if (policy.registrationEnabled) {
      node.append(
        actionButton("navSignIn", "/login", "ghost", big),
        actionButton("navCreateAccount", "/register", "primary", big)
      );
    } else {
      node.append(actionButton("navSignIn", "/login", "primary", big));
    }
  }

  function renderNote() {
    const node = byId("heroNote");
    if (!node) return;
    let key = "";
    let vars = null;
    if (user) {
      key = "landingSignedInAs";
      vars = { email: user.email || "" };
    } else if (!policyState.known) {
      key = "landingPolicyUnknown";
    } else if (!policyState.policy.registrationEnabled) {
      key = "landingClosedNote";
    } else if (!policyState.policy.passwordLoginEnabled && !policyState.policy.codeLoginEnabled) {
      key = "signInUnavailableBody";
    } else {
      key = "heroCtaNote";
    }
    setPipeText(node, t(key, vars));
    show(node, true);
  }

  function render() {
    drawActions(byId("navAuth"), false);
    drawActions(byId("heroCta"), true);
    drawActions(byId("ctaBandActions"), true);
    drawActions(byId("footAuth"), false);
    renderNote();
  }

  let done = false;
  const ready = (async () => {
    try {
      const [me, policy] = await Promise.all([fetchMe(), fetchPolicy()]);
      user = me.state === "in" ? me.user : null;
      policyState = policy;
      render();
    } finally {
      done = true;
    }
  })();

  onChange(render);

  window.magpieCloud.landing = {
    policy: () => policyState,
    user: () => user,
    ready: ready,
    done: () => done
  };
})();
