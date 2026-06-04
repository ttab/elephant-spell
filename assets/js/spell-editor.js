// spell-editor.js — progressive enhancement for the dictionary entry form.
//
// It registers a small code-input template that highlights the {A|B} expansion
// syntax used in common mistakes (and, later, rule patterns), wires the
// add/remove buttons for the structured "forms" rows, and shows a live,
// server-validated preview of how each common-mistakes pattern expands.
//
// All behaviour is attached through event delegation on the document so it
// keeps working for form markup swapped in by htmx.
(function () {
  "use strict";

  // --- syntax highlighting -------------------------------------------------

  function escapeHtml(text) {
    return text
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
  }

  // highlightPattern colours the {A|B} expansion syntax used by common mistakes.
  function highlightPattern(text) {
    return escapeHtml(text)
      .replace(/[{}]/g, function (m) {
        return '<span class="tok-brace">' + m + "</span>";
      })
      .replace(/\|/g, '<span class="tok-pipe">|</span>');
  }

  // highlightRule colours the rule DSL: {digit}/{word}/{gap(N)} placeholders and
  // {1}/{2} capture references.
  function highlightRule(text) {
    return escapeHtml(text)
      .replace(/\{(?:digit|word|gap(?:\(\d+\))?)\}/g, function (m) {
        return '<span class="tok-rule">' + m + "</span>";
      })
      .replace(/\{\d+\}/g, function (m) {
        return '<span class="tok-cap">' + m + "</span>";
      });
  }

  // ruleTokens are the placeholders offered by autocomplete in a rule pattern.
  var ruleTokens = ["digit", "word", "gap", "gap(4)"];

  // ruleAutocomplete fills the popup with matching token completions when the
  // caret sits just after an unclosed "{…".
  function ruleAutocomplete(popup, textarea) {
    var end = textarea.selectionEnd;
    var before = textarea.value.slice(0, end);
    var m = /\{([a-zA-Z(]*)$/.exec(before);

    if (!m) {
      popup.innerHTML = "";

      return;
    }

    var partial = m[1].toLowerCase();
    var matches = ruleTokens.filter(function (t) {
      return t.indexOf(partial) === 0;
    });

    if (!matches.length) {
      popup.innerHTML = "";

      return;
    }

    popup.dataset.from = m.index;
    popup.dataset.to = end;
    popup.innerHTML = matches
      .map(function (t) {
        return (
          '<button type="button" class="ac-item" data-token="' +
          t +
          '">{' +
          t +
          "}</button>"
        );
      })
      .join("");
  }

  if (window.codeInput) {
    codeInput.registerTemplate("spell-pattern", {
      highlight: function (resultElement, codeInputElement) {
        resultElement.innerHTML = highlightPattern(codeInputElement.value);
      },
      includeCodeInputInHighlightFunc: true,
      preElementStyled: true,
      isCode: false,
      plugins: [],
    });

    var rulePlugins = [];
    if (codeInput.plugins && codeInput.plugins.Autocomplete) {
      rulePlugins.push(new codeInput.plugins.Autocomplete(ruleAutocomplete));
    }

    codeInput.registerTemplate("spell-rule", {
      highlight: function (resultElement, codeInputElement) {
        resultElement.innerHTML = highlightRule(codeInputElement.value);
      },
      includeCodeInputInHighlightFunc: true,
      preElementStyled: true,
      isCode: false,
      plugins: rulePlugins,
    });

    codeInput.registerTemplate("spell-rule-repl", {
      highlight: function (resultElement, codeInputElement) {
        resultElement.innerHTML = highlightRule(codeInputElement.value);
      },
      includeCodeInputInHighlightFunc: true,
      preElementStyled: true,
      isCode: false,
      plugins: [],
    });
  }

  // Insert the chosen token completion, replacing the partial "{…" before the
  // caret. Use mousedown + preventDefault so the textarea keeps focus/caret.
  document.addEventListener("mousedown", function (e) {
    var item = e.target.closest(".ac-item");
    if (!item) {
      return;
    }

    e.preventDefault();

    var popup = item.closest(".code-input_autocomplete_popup");
    var ci = item.closest("code-input");
    var ta = ci && ci.querySelector("textarea");
    if (!popup || !ta) {
      return;
    }

    var from = parseInt(popup.dataset.from, 10);
    var to = parseInt(popup.dataset.to, 10);
    var token = "{" + item.getAttribute("data-token") + "}";

    ta.value = ta.value.slice(0, from) + token + ta.value.slice(to);

    var pos = from + token.length;
    ta.focus();
    ta.setSelectionRange(pos, pos);
    ta.dispatchEvent(new Event("input", { bubbles: true }));

    popup.innerHTML = "";
  });

  // --- forms rows ----------------------------------------------------------

  document.addEventListener("click", function (e) {
    var add = e.target.closest("[data-add-form-row]");
    if (add) {
      e.preventDefault();

      var tpl = document.getElementById("forms-row-template");
      var rows = document.getElementById("forms-rows");
      if (tpl && rows) {
        rows.appendChild(tpl.content.cloneNode(true));
        var inputs = rows.querySelectorAll(".forms-row:last-child input");
        if (inputs.length) {
          inputs[0].focus();
        }
      }
      return;
    }

    var remove = e.target.closest("[data-row-remove]");
    if (remove) {
      e.preventDefault();
      var row = remove.closest(".forms-row");
      if (row) {
        row.remove();
      }
    }
  });

  // --- common-mistakes validation preview ----------------------------------

  var timers = new WeakMap();

  function scheduleValidate(el) {
    clearTimeout(timers.get(el));
    timers.set(
      el,
      setTimeout(function () {
        validate(el);
      }, 400)
    );
  }

  function validate(el) {
    var url = el.getAttribute("data-validate-url");
    var targetSel = el.getAttribute("data-preview-target");
    if (!url || !targetSel) {
      return;
    }

    var target = document.querySelector(targetSel);
    if (!target) {
      return;
    }

    var body = new URLSearchParams();
    body.set("common_mistakes", el.value);

    fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "HX-Request": "true",
      },
      body: body.toString(),
      credentials: "same-origin",
    })
      .then(function (r) {
        return r.ok ? r.text() : Promise.reject(r.status);
      })
      .then(function (html) {
        target.innerHTML = html;
      })
      .catch(function () {
        /* leave the last preview in place on transient errors */
      });
  }

  document.addEventListener("input", function (e) {
    var el =
      e.target.closest && e.target.closest("code-input[data-validate-url]");
    if (el) {
      scheduleValidate(el);
    }
  });

  // --- expansions modal ----------------------------------------------------

  function openExpansions(trigger) {
    var group = trigger.closest(".form-group");
    var editor = group && group.querySelector("code-input[data-expansions-url]");
    var modal = document.getElementById("expansion-modal");
    var body = document.getElementById("expansion-modal-body");
    if (!editor || !modal || !body) {
      return;
    }

    var data = new URLSearchParams();
    data.set("common_mistakes", editor.value);

    fetch(editor.getAttribute("data-expansions-url"), {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
        "HX-Request": "true",
      },
      body: data.toString(),
      credentials: "same-origin",
    })
      .then(function (r) {
        return r.ok ? r.text() : Promise.reject(r.status);
      })
      .then(function (html) {
        body.innerHTML = html;
        modal.showModal();
      })
      .catch(function () {
        /* ignore transient errors */
      });
  }

  document.addEventListener("click", function (e) {
    // Move the active highlight in the list immediately on click; the detail
    // pane is swapped by htmx but the list itself isn't re-rendered until a
    // full page load, so the server-rendered active state would otherwise lag.
    var item = e.target.closest(".entry-item");
    if (item) {
      var list = item.closest("#entry-list") || document;
      list.querySelectorAll(".entry-item.active").forEach(function (el) {
        el.classList.remove("active");
      });
      item.classList.add("active");
      // fall through — htmx still handles the navigation.
    }

    var open = e.target.closest("[data-open-expansions]");
    if (open) {
      e.preventDefault();
      openExpansions(open);
      return;
    }

    if (e.target.closest("[data-close-modal]")) {
      var modal = document.getElementById("expansion-modal");
      if (modal && modal.open) {
        modal.close();
      }
      return;
    }

    // Close when clicking the backdrop (outside the dialog content).
    if (e.target.id === "expansion-modal") {
      e.target.close();
    }
  });
})();
