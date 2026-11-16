// The recall game on the home page: the peanut story in steps.
// The terminal output and answers were recorded from a real run of the
// cheesedb command (nomic ModernBERT embeddings, a Qwen chat model, both
// local). Edit STEPS to change the story; render() draws everything from it.
(function () {
  var $ = function (s) { return document.querySelector(s); };
  if (!$('#game')) return;

  // Lots 1-3 and memories 4-6, as in the terminal.
  var NODES = [1, 2, 3, 4, 5, 6];
  var EDGES = [['e1', 1, 4], ['e2', 2, 4], ['e3', 3, 5], ['e4', 4, 6], ['e5', 5, 6]];
  var SUB = {1: 'allergic to peanuts', 2: 'allergy confirmed', 3: 'sauce is peanut-free',
             4: 'allergic to peanuts', 5: 'sauce has no peanuts', 6: 'satay is safe'};

  function cmd(c) { return '<span class="p">$ cheesedb ' + c + '</span>\n'; }
  function dim(t) { return '<span class="d">' + t + '</span>\n'; }

  var SEARCH = 'search story "can the user eat the satay at Spice Garden?"';
  var ASK = 'ask story "Can the user eat the satay at Spice Garden?"';

  var STEPS = [
    {title: 'Three sources arrive',
     text: 'An assistant hears from the user, reads a doctor’s note, and finds a restaurant’s web page. Each one is stored as a source (a “lot”) with a number.',
     show: [1, 2, 3],
     term: cmd('lot story user_message "I am allergic to peanuts"') + 'lot 1\n' +
           cmd('lot story doctor_note "Allergy test result: peanut allergy confirmed"') + 'lot 2\n' +
           cmd('lot story web_page "Spice Garden menu: our satay sauce is peanut-free"') + 'lot 3\n'},
    {title: 'It learns, and writes down where from',
     text: 'Every memory names its sources. #4 came from the user OR the doctor. #5 came from the web page. #6 is a conclusion that needs #4 AND #5.',
     term: cmd('remember story "The user is allergic to peanuts" 1 2') + 'memory 4\n' +
           cmd('remember story "Spice Garden’s satay sauce has no peanuts" 3') + 'memory 5\n' +
           cmd('remember story "The satay at Spice Garden is safe for the user" 4,5') + 'memory 6\n'},
    {title: 'Ask in plain English',
     text: 'Search finds memories by meaning, not by keywords. Then a chat model answers from those memories only. The green boxes are what search found.',
     hits: [4, 5, 6],
     term: cmd(SEARCH) + '6: The satay at Spice Garden is safe for the user (match 0.81)\n' +
           '5: Spice Garden’s satay sauce has no peanuts (match 0.77)\n' +
           '4: The user is allergic to peanuts (match 0.57)\n' + cmd(ASK),
     bubble: 'Yes, the user can eat the satay at Spice Garden because the satay sauce there contains no peanuts, which addresses the user’s peanut allergy.'},
    {title: 'Quiz: the web page was wrong',
     text: 'The satay sauce does contain peanuts. Which memories should go? Tap them, then press Check.',
     quiz: 'pick',
     term: dim('# The model is about to tell an allergic user to eat peanuts.') +
           dim('# Follow the arrows down from source 3.')},
    {title: 'Press Recall',
     text: 'One command removes the web page and everything learned from it. Erase also wipes their text from the files.',
     button: true,
     term: cmd('recall story 3 fair erase')},
    {title: 'Ask again',
     text: 'Search and the model no longer see the bad memories. The model now says it does not know, instead of guessing.',
     hits: [4],
     term: cmd(SEARCH) + '4: The user is allergic to peanuts (match 0.57)\n' + cmd(ASK),
     bubble: 'I do not know if the satay at Spice Garden contains peanuts, so I cannot determine if it is safe for the user to eat.'},
    {title: 'Check the proof',
     text: 'The text is gone from the disk, not just hidden: a byte-by-byte search of every file finds nothing. The history still verifies.',
     term: cmd('find story "peanut-free"') +
           '"peanut-free": not found anywhere in ~/.cheesedb/data/73746f7279\n(searched every file, byte by byte)\n' +
           cmd('verify story') + '<span class="g">OK.</span> head fingerprint: 04bc8e740a539ad1…\n'},
    {title: 'Quiz: true facts survive',
     text: 'Now the user’s own message (source 1) is recalled too. Does memory #4, “allergic to peanuts”, survive?',
     quiz: 'yesno',
     term: cmd('recall story 1 fair erase')},
    {title: 'You did it',
     text: 'Every memory knows its sources. One recall removed a bad source and everything built on it: from search, from the AI’s answers, and from the disk. A fact that still has a good source stayed.',
     done: true,
     term: dim('# Try it on your machine:') +
           cmd('agent create story') + dim('# …then the commands from each step.') +
           dim('# Or give Claude Code the same memory:') +
           '<span class="p">$ claude mcp add cheesedb -- cheesedb mcp story</span>\n'}
  ];

  var s = {step: 0, solved1: false, pressed: false, solved2: false, picks: {}};

  function dead() {
    var d = {};
    if (s.step >= 5 || (s.step === 4 && s.pressed)) { d[3] = d[5] = d[6] = true; }
    if (s.step >= 8 || (s.step === 7 && s.solved2)) { d[1] = true; }
    return d;
  }

  function render() {
    var st = STEPS[s.step], d = dead(), erased = s.step >= 4;
    $('#gameNo').textContent = 'Step ' + (s.step + 1) + ' of ' + STEPS.length;
    $('#gameTitle').textContent = st.title;
    $('#gameText').textContent = st.text;
    $('#gameDots').innerHTML = STEPS.map(function (_, i) { return '<span class="' + (i <= s.step ? 'on' : '') + '"></span>'; }).join('');

    NODES.forEach(function (id) {
      var g = $('#g' + id);
      g.classList.toggle('hid', s.step === 0 && id > 3);
      g.classList.toggle('dead', !!d[id]);
      g.classList.toggle('hit', (st.hits || []).indexOf(id) !== -1 && !d[id]);
      g.classList.toggle('pickable', st.quiz === 'pick' && id > 3);
      g.classList.toggle('pick', st.quiz === 'pick' && !!s.picks[id]);
      g.querySelector('.s').textContent = d[id] && erased ? 'text erased' : SUB[id];
      // Memories are buttons only during the quiz; a screen reader hears
      // each node's state, since the colours alone do not say it.
      var pick = st.quiz === 'pick' && id > 3 && !s.solved1;
      if (pick) {
        g.setAttribute('role', 'button');
        g.setAttribute('tabindex', '0');
        g.setAttribute('aria-pressed', s.picks[id] ? 'true' : 'false');
      } else {
        g.removeAttribute('role');
        g.removeAttribute('tabindex');
        g.removeAttribute('aria-pressed');
      }
      var name = (id > 3 ? 'memory ' : 'source ') + id + ': ' + SUB[id];
      var state = s.step === 0 && id > 3 ? 'not learned yet' : d[id] ? (erased ? 'recalled, text erased' : 'recalled') : 'alive';
      g.setAttribute('aria-label', name + ', ' + state);
    });
    EDGES.forEach(function (e) {
      var p = $('#' + e[0]);
      p.classList.toggle('hid', s.step === 0);
      p.classList.toggle('off', !!(d[e[1]] || d[e[2]]));
    });

    var term = st.term;
    if (st.button && s.pressed) term += 'recalled 3; memories removed: [5 6]\n';
    if (st.quiz === 'yesno' && s.solved2) {
      term += 'recalled 1; memories removed: []\n' + cmd('active story') +
        '[4] memory alive "The user is allergic to peanuts"\n';
    }
    $('#gameTerm').innerHTML = term;
    var b = $('#gameBubble');
    b.hidden = !st.bubble;
    if (st.bubble) b.lastChild.textContent = st.bubble;

    var q = $('#gameQuiz');
    q.innerHTML = '';
    if (st.quiz === 'pick') {
      q.innerHTML = '<button type="button" data-a="check">Check</button><button type="button" data-a="show">Show me</button>';
    } else if (st.quiz === 'yesno' && !s.solved2) {
      q.innerHTML = '<button type="button" data-a="yes">Yes, it survives</button><button type="button" data-a="no">No, it goes</button>';
    } else if (st.button) {
      q.innerHTML = '<button class="recall gamebtn" type="button" data-a="recall"' + (s.pressed ? ' disabled' : '') + '>Recall<small>source 3</small></button>';
    } else if (st.done) {
      q.innerHTML = '<button type="button" data-a="again">Play again</button>';
    }
    if (!st.quiz && !st.button) setFeedback('', '');

    $('#gameBack').disabled = s.step === 0;
    var blocked = (st.quiz === 'pick' && !s.solved1) || (st.button && !s.pressed) || (st.quiz === 'yesno' && !s.solved2);
    $('#gameNext').disabled = blocked || !!st.done;
  }

  function setFeedback(cls, text) {
    var f = $('#gameFeedback');
    f.className = 'feedback ' + cls;
    f.textContent = text;
  }

  function answer(a) {
    if (a === 'check' || a === 'show') {
      var right = s.picks[5] && s.picks[6] && !s.picks[4];
      if (a === 'show') { s.picks = {5: true, 6: true}; right = true; }
      if (right) {
        s.solved1 = true;
        setFeedback('ok', 'Right: #5 came only from the web page, and #6 needs #5. #4 stays, because the user and the doctor still back it.');
      } else if (s.picks[4]) {
        setFeedback('no', 'Not #4: it came from the user or the doctor, and both are still fine. Try again.');
      } else {
        setFeedback('no', 'Not quite. Follow the arrows down from source 3, all the way. Try again.');
      }
    } else if (a === 'recall') {
      s.pressed = true;
      setFeedback('ok', 'Recalled: source 3, memory #5 and memory #6 are gone, text included.');
    } else if (a === 'yes' || a === 'no') {
      s.solved2 = true;
      setFeedback(a === 'yes' ? 'ok' : 'no', (a === 'yes' ? 'Right! ' : 'It survives! ') +
        '#4 has two ways it was learned: the user OR the doctor. A memory lives while any one of them holds (“fair” recall). With “strict” recall it would go.');
    } else if (a === 'again') {
      s = {step: 0, solved1: false, pressed: false, solved2: false, picks: {}};
      setFeedback('', '');
    }
    render();
  }

  $('#gameQuiz').addEventListener('click', function (ev) {
    var btn = ev.target.closest('button');
    if (btn) answer(btn.dataset.a);
  });
  [4, 5, 6].forEach(function (id) {
    var g = $('#g' + id);
    var toggle = function () {
      if (STEPS[s.step].quiz !== 'pick' || s.solved1) return;
      s.picks[id] = !s.picks[id];
      render();
    };
    g.addEventListener('click', toggle);
    g.addEventListener('keydown', function (ev) {
      if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); toggle(); }
    });
  });
  function go(by) {
    var to = s.step + by;
    if (to < 0 || to >= STEPS.length) return;
    s.step = to;
    setFeedback('', '');
    render();
  }
  $('#gameNext').addEventListener('click', function () { go(1); });
  $('#gameBack').addEventListener('click', function () { go(-1); });
  render();
})();
