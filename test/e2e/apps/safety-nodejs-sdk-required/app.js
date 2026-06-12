'use strict';
const express = require('express');
const app = express();

app.get('/', (req, res) => {
    res.json({ message: 'hello from node express' });
});

app.listen(8080, () => {
    console.log('listening on 8080');
});
